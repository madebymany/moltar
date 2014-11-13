package main

import (
	"bufio"
	"code.google.com/p/gosshold/ssh"
	"errors"
	"fmt"
	"io"
	"launchpad.net/goamz/aws"
	"launchpad.net/goamz/ec2"
	"log"
	"os"
	"os/exec"
	"path"
	"strings"
	"syscall"
)

var ErrNoInstancesFound = errors.New("No instances found; run provisioner first")

const AfterDeployHookScript = ".moltar-after-deploy"
const FailedDeployHookScript = ".moltar-failed-deploy"

type ExecError struct {
	instance ec2.Instance
	error    error
}

func (f ExecError) Error() string {
	return fmt.Sprintf("%s : %s", instanceLogName(&f.instance), f.error)
}

type Job struct {
	region                  aws.Region
	env                     string
	cluster                 string
	project                 string
	packageNames            []string
	instances               []*ec2.Instance
	instanceSshClients      map[*ec2.Instance]*ssh.ClientConn
	instanceLoggers         map[*ec2.Instance]*log.Logger
	output                  io.Writer
	logger                  *log.Logger
	installVersionRev       uint64
	shouldOutputAnsiEscapes bool
}

func getInstancesTagged(ec2client *ec2.EC2, project string, env string, cluster string, packageName string) (instances []*ec2.Instance, err error) {
	instanceFilter := ec2.NewFilter()
	instanceFilter.Add("instance-state-name", "running")
	instanceFilter.Add("tag:Project", project)
	queryEnv := env
	if env == "" {
		queryEnv = "*"
	}
	instanceFilter.Add("tag:Environment", queryEnv)
	if cluster != "" {
		instanceFilter.Add("tag:Cluster", cluster)
	}

	if packageName != "" {
		instanceFilter.Add("tag:Packages", "*|"+packageName+"|*")
	}

	instancesResp, err := ec2client.Instances(nil, instanceFilter)
	if err != nil {
		return
	}

	instances = make([]*ec2.Instance, 0, 20)
	for _, res := range instancesResp.Reservations {
		for _, inst := range res.Instances {
			newInst := inst
			instances = append(instances, &newInst)
		}
	}

	return instances, nil
}

func NewJob(awsConf AWSConf, env string, cluster string, project string, packageNames []string, searchPackageNames []string, output io.Writer, shouldOutputAnsiEscapes bool) (job *Job, err error) {
	e := ec2.New(awsConf.Auth, awsConf.Region)

	if searchPackageNames == nil || len(searchPackageNames) == 0 {
		searchPackageNames = []string{""}
	}

	instancesSet := map[string]*ec2.Instance{}
	instancesCount := map[string]int{}
	for _, packageName := range searchPackageNames {
		instances, err := getInstancesTagged(e, project, env, cluster, packageName)
		if err != nil {
			return nil, err
		}

		for _, instance := range instances {
			instancesSet[instance.InstanceId] = instance
			instancesCount[instance.InstanceId] += 1
		}
	}

	instances := make([]*ec2.Instance, 0, len(instancesSet))
	for _, instance := range instancesSet {
		if instancesCount[instance.InstanceId] == len(searchPackageNames) {
			instances = append(instances, instance)
		}
	}

	if len(instances) == 0 {
		return nil, ErrNoInstancesFound
	}

	logger := log.New(output, "", 0)

	return &Job{region: awsConf.Region, env: env, cluster: cluster,
		project: project, packageNames: packageNames, instances: instances,
		instanceSshClients: make(map[*ec2.Instance]*ssh.ClientConn),
		instanceLoggers:    make(map[*ec2.Instance]*log.Logger),
		output:             output, logger: logger,
		shouldOutputAnsiEscapes: shouldOutputAnsiEscapes}, nil
}

func (self *Job) Exec(cmd string, series bool) (errs []error) {
	execErrs := make([]ExecError, 0, len(self.instances))
	if series {
		execErrs = self.execInSeries(cmd)
	} else {
		execErrs = self.execInParallel(cmd)
	}
	if len(execErrs) > 0 {
		for _, execErr := range execErrs {
			errs = append(errs, execErr)
		}
	}
	return
}

func (self *Job) Deploy(runHooks bool, parallel bool) (errs []error) {
	execErrs := make([]ExecError, 0, len(self.instances))
	execErrs = self.execList([]string{
		"sudo apt-get update -qq",
		"sudo DEBIAN_FRONTEND=noninteractive apt-get install -qy '" +
			strings.Join(self.packageNames, "' '") + "'",
		"sudo DEBIAN_FRONTEND=noninteractive apt-get autoremove -yq",
		"sudo apt-get clean -yq",
	}, parallel)

	hosts := make([]string, 0, len(execErrs))
	for _, execErr := range execErrs {
		hosts = append(hosts, execErr.instance.DNSName)
		errs = append(errs, execErr)
	}

	if runHooks {
		if _, err := os.Stat(FailedDeployHookScript); len(execErrs) > 0 && err == nil {
			fmt.Println(getHookMessage(FailedDeployHookScript))
			runHook(FailedDeployHookScript,
				[]string{"ENV=" + self.env, "FAILED_HOSTS=" + strings.Join(hosts, " ")})
		} else if _, err := os.Stat(AfterDeployHookScript); err == nil {
			fmt.Println(getHookMessage(AfterDeployHookScript))
			runHook(AfterDeployHookScript, []string{"ENV=" + self.env})
		}
	}
	return
}

func (self *Job) Ssh(criteria string, sshArgs []string) (err error) {
	sshPath, err := exec.LookPath("ssh")
	if err != nil {
		return
	}

	var instance *ec2.Instance
	matches := self.instances

	if criteria != "-1" {
		if criteria != "" {
			matches = make([]*ec2.Instance, 0, len(self.instances))
			for _, instance = range self.instances {
				if matchCriteria(instance, criteria) {
					instanceLogName(instance)
					matches = append(matches, instance)
				}
			}
		}

		if len(matches) == 0 {
			self.logger.Fatalf("Instance '%s' not found\n", criteria)
		} else if len(matches) > 1 {
			self.logger.Printf("Multiple matches for '%s' found:\n", criteria)
			self.printInstances(matches)
			self.logger.Fatal("")
		}
	}

	instance = matches[0]

	execArgs := []string{"ssh"}
	keyFile := self.keyFile()
	if keyFile != "" {
		execArgs = append(execArgs, "-i", keyFile)
	}

	execArgs = append(execArgs,
		fmt.Sprintf("%s@%s", self.sshUserName(instance), instance.DNSName))
	execArgs = append(execArgs, sshArgs...)

	fPrintShellCommand(self.output, "", execArgs)
	fmt.Fprintln(self.output, "")

	prepareExec()
	err = syscall.Exec(sshPath, execArgs, os.Environ())
	return
}

func (self *Job) Scp(args []string) (err error) {
	scpPath, err := exec.LookPath("scp")
	if err != nil {
		return
	}

	defaultArgs := []string{"-q"}
	keyFile := self.keyFile()
	if keyFile != "" {
		defaultArgs = append(defaultArgs, []string{
			"-i", keyFile,
		}...)
	}
	scpArgs := make([]string, len(defaultArgs)+len(args))
	copy(scpArgs, defaultArgs)
	copy(scpArgs[len(defaultArgs):], args)

	var dstIndex = -1
	for i, arg := range scpArgs {
		if arg[0] == ':' {
			dstIndex = i
			break
		}
	}
	if dstIndex == -1 {
		dstIndex = len(scpArgs)
		scpArgs = append(scpArgs, ":")
	}

	errChan := make(chan error, len(self.instances))

	for _, instance := range self.instances {
		go func(instance *ec2.Instance) {
			var err error
			args := make([]string, len(scpArgs))
			copy(args, scpArgs)

			logger := self.instanceLogger(instance)
			args[dstIndex] = fmt.Sprintf("%s@%s%s",
				self.sshUserName(instance), instance.DNSName, args[dstIndex])

			fPrintShellCommand(self.output, "scp", args)

			cmd := exec.Command(scpPath, args...)
			outPipeRead, outPipeWrite, err := os.Pipe()
			if err != nil {
				logger.Printf("error creating pipe: %s\n", err)
				errChan <- err
				return
			}
			cmd.Stdout = outPipeWrite
			cmd.Stderr = outPipeWrite

			err = cmd.Start()
			if err != nil {
				logger.Printf("error starting scp: %s\n", err)
				errChan <- err
				return
			}

			outPipeWrite.Close()
			stdoutReader := bufio.NewReader(outPipeRead)
			for {
				in, err := stdoutReader.ReadString('\n')
				if (err == io.EOF && in != "") || err == nil {
					logger.Print(in)
				}
				if err != nil {
					break
				}
			}

			err = cmd.Wait()
			outPipeRead.Close()
			if err != nil {
				logger.Printf("error running scp: %s\n", err)
			}
			errChan <- err
		}(instance)
	}

	var scpErr error
	for _ = range self.instances {
		scpErr = <-errChan
		if err == nil && scpErr != nil {
			err = errors.New("at least one scp failed")
		}
	}
	return
}

func (self *Job) List() (err error) {
	self.printInstances(self.instances)
	return nil
}

func (self *Job) Hostname(instanceName string) (err error) {
	for _, instance := range self.instances {
		if instanceLogName(instance) == instanceName {
			fmt.Fprintln(self.output, instance.DNSName)
			return nil
		}
	}
	return errors.New(instanceName + " not found")
}

/// Subtasks

func (self *Job) sshClient(i *ec2.Instance) (conn *ssh.ClientConn, err error) {
	conn = self.instanceSshClients[i]
	if conn == nil {
		conn, err = self.sshDial(i)
		if err == nil {
			self.instanceSshClients[i] = conn
		}
	}
	return
}

func (self *Job) instanceLogger(i *ec2.Instance) (logger *log.Logger) {
	logger = self.instanceLoggers[i]
	if logger == nil {
		prefix := instanceLogName(i)
		if self.shouldOutputAnsiEscapes {
			prefix = "\033[1m" + prefix + "\033[0m"
		}
		logger = log.New(self.output, prefix+" ", 0)
		self.instanceLoggers[i] = logger
	}
	return
}

func (self *Job) exec(instance ec2.Instance, cmd string, errChan chan ExecError) {
	conn, err := self.sshClient(&instance)
	if err != nil {
		errChan <- ExecError{error: err, instance: instance}
		return
	}

	logger := self.instanceLogger(&instance)
	_, returnChan, err := sshRunOutLogger(conn, cmd, logger, nil)
	if err == nil {
		err = <-returnChan
	}
	errChan <- ExecError{error: err, instance: instance}
	startStdinRead()
	return
}

func (self *Job) execInSeries(cmd string) (errs []ExecError) {
	errChan := make(chan ExecError, len(self.instances))
	errs = make([]ExecError, 0, len(self.instances))

	for _, instance := range self.instances {
		self.exec(*instance, cmd, errChan)
	}

	for _ = range self.instances {
		if err := <-errChan; err.error != nil {
			errs = append(errs, err)
		}
	}
	return
}

func (self *Job) execInParallel(cmd string) (errs []ExecError) {
	errChan := make(chan ExecError, len(self.instances))
	errs = make([]ExecError, 0, len(self.instances))

	for _, instance := range self.instances {
		go func(inst ec2.Instance) {
			self.exec(inst, cmd, errChan)
		}(*instance)
	}

	for _ = range self.instances {
		if err := <-errChan; err.error != nil {
			errs = append(errs, err)
		}
	}
	return
}

func (self *Job) execList(cmds []string, series bool) (errs []ExecError) {
	for _, cmd := range cmds {
		fmt.Printf("\n%s\n\n", cmd)
		if series {
			errs = self.execInSeries(cmd)
		} else {
			errs = self.execInParallel(cmd)
		}
		if len(errs) > 0 {
			return
		}
	}
	return []ExecError{}
}

func (self *Job) keyFile() (path string) {
	fileName := self.project
	if len(self.packageNames) > 0 {
		fileName += fmt.Sprintf("-%s", self.packageNames[0])
	}
	path = fmt.Sprintf(os.ExpandEnv("${HOME}/Google Drive/%s Ops/Keys/%s.pem"),
		self.project, fileName)

	if _, err := os.Stat(path); err == nil {
		return path
	} else {
		return ""
	}
}

func (self *Job) sshUserName(_ *ec2.Instance) (userName string) {
	// TODO: be more clever about this
	return "ubuntu"
}

func (self *Job) sshDial(i *ec2.Instance) (conn *ssh.ClientConn, err error) {
	conn, err = sshDial(i.DNSName+":22", self.sshUserName(i), self.keyFile())
	return
}

func (self *Job) printInstances(instances []*ec2.Instance) {
	fields := make([][]string, len(instances))
	for i, instance := range instances {
		fields[i] = []string{instance.InstanceId, instanceLogName(instance),
			instance.DNSName}
	}
	fmt.Fprint(self.output, formatTable(fields))
}

func instanceLogName(i *ec2.Instance) string {
	for _, tag := range i.Tags {
		if tag.Key == "Name" && tag.Value != "" {
			return tag.Value
		}
	}
	return i.InstanceId
}

func fPrintShellCommand(w io.Writer, n string, cmd []string) {
	if n != "" {
		fmt.Fprintf(w, "%s ", n)
	}
	for i, cmdPart := range cmd {
		// TODO: this escaping will work most of the time, but isn't that great
		if strings.ContainsAny(cmdPart, " $") {
			fmt.Fprintf(w, "'%s'", cmdPart)
		} else {
			fmt.Fprint(w, cmdPart)
		}
		if i < (len(cmd) - 1) {
			fmt.Fprint(w, " ")
		}
	}
	fmt.Fprint(w, "\n")
}

func runHook(script_path string, environment []string) {
	prepareExec()
	pwd, err := os.Getwd()
	if err != nil {
		return
	}
	vars := make([]string, 0, len(os.Environ()))
	for _, env := range environment {
		vars = append(vars, env)
	}
	for _, env := range os.Environ() {
		vars = append(vars, env)
	}
	syscall.Exec(path.Join(pwd, script_path),
		[]string{script_path},
		vars)

}

func matchCriteria(instance *ec2.Instance, criteria string) bool {
	var found bool
	for _, value := range strings.Split(criteria, "/") {
		found = false
		for _, tag := range instance.Tags {
			if strings.Contains(tag.Value, value) {
				found = true
				break
			}
		}
		if !strings.Contains(instance.InstanceId, value) && !strings.Contains(instance.PrivateDNSName, value) && !strings.Contains(instance.DNSName, value) && found == false {
			return false
		}
	}
	return true
}

func getHookMessage(script string) string {
	return fmt.Sprintf("Running deploy failure hook %s", script)
}

func prepareExec() {
	/* There appears to be a bug with goamz where some fds are left open, and
	 * just closing them causes a crash. If we ask all fds > 2 to close on
	 * exec, all is well.
	 */
	var rlimit syscall.Rlimit
	err := syscall.Getrlimit(syscall.RLIMIT_NOFILE, &rlimit)
	if err != nil {
		panic(err)
	}
	maxFds := int(rlimit.Cur)
	for fd := 3; fd < maxFds; fd++ {
		syscall.CloseOnExec(fd)
	}
}
