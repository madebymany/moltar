package main

import (
	"bufio"
	"errors"
	"fmt"
	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/ec2"
	"golang.org/x/crypto/ssh"
	"io"
	"log"
	"os"
	"os/exec"
	"strings"
	"syscall"
)

var ErrNoInstancesFound = errors.New("No instances found; run provisioner first")

const AfterDeployHookScript = ".moltar-after-deploy"
const FailedDeployHookScript = ".moltar-failed-deploy"

type ExecError struct {
	instance ec2.Instance
	err      error
}

func (f ExecError) Error() string {
	return fmt.Sprintf("%s : %s", instanceLogName(&f.instance), f.err)
}

type Job struct {
	env                     string
	cluster                 string
	project                 string
	packageNames            []string
	instances               []*ec2.Instance
	instanceSshClients      map[*ec2.Instance]*ssh.Client
	instanceLoggers         map[*ec2.Instance]*log.Logger
	output                  io.Writer
	logger                  *log.Logger
	installVersionRev       uint64
	shouldOutputAnsiEscapes bool
}

func getInstancesTagged(svc *ec2.EC2, project string, env string, cluster string, packageName string) (instances []*ec2.Instance, err error) {
	filters := make([]*ec2.Filter, 0)
	filters = append(filters, &ec2.Filter{
		Name: aws.String("instance-state-name"),
		Values: []*string{
			aws.String("running"),
		},
	})

	filters = append(filters, &ec2.Filter{
		Name: aws.String("instance-state-name"),
		Values: []*string{
			aws.String("running"),
		},
	})
	queryEnv := env
	if env == "" {
		queryEnv = "*"
	}
	filters = append(filters, &ec2.Filter{
		Name: aws.String("tag:Environment"),
		Values: []*string{
			aws.String(queryEnv),
		},
	})
	if cluster != "" {
		filters = append(filters, &ec2.Filter{
			Name: aws.String("tag:Cluster"),
			Values: []*string{
				aws.String(cluster),
			},
		})
	}

	if packageName != "" {
		filters = append(filters, &ec2.Filter{
			Name: aws.String("tag:Packages"),
			Values: []*string{
				aws.String("*|" + packageName + "|*"),
			},
		})
	}
	params := &ec2.DescribeInstancesInput{
		Filters: filters,
	}

	resp, err := svc.DescribeInstances(params)
	if err != nil {
		return
	}

	instances = make([]*ec2.Instance, 0, 20)
	for _, res := range resp.Reservations {
		for _, inst := range res.Instances {
			newInst := inst
			instances = append(instances, newInst)
		}
	}

	return instances, nil
}

func NewJob(session *session.Session, env string, cluster string, project string, packageNames []string, searchPackageNames []string, output io.Writer, shouldOutputAnsiEscapes bool) (job *Job, err error) {
	e := ec2.New(session)

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
			instancesSet[*instance.InstanceId] = instance
			instancesCount[*instance.InstanceId] += 1
		}
	}

	instances := make([]*ec2.Instance, 0, len(instancesSet))
	for _, instance := range instancesSet {
		if instancesCount[*instance.InstanceId] == len(searchPackageNames) {
			instances = append(instances, instance)
		}
	}

	if len(instances) == 0 {
		return nil, ErrNoInstancesFound
	}

	logger := log.New(output, "", 0)

	return &Job{env: env, cluster: cluster,
		project: project, packageNames: packageNames, instances: instances,
		instanceSshClients: make(map[*ec2.Instance]*ssh.Client),
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

func (self *Job) Deploy(runHooks bool, series bool) (errs []error) {
	execErrs := make([]ExecError, 0, len(self.instances))
	execErrs = self.execList([]string{
		"sudo apt-get update -qq",
		"sudo DEBIAN_FRONTEND=noninteractive apt-get install -qy '" +
			strings.Join(self.packageNames, "' '") + "'",
		"sudo DEBIAN_FRONTEND=noninteractive apt-get autoremove -yq",
		"sudo apt-get clean -yq",
	}, series)

	hosts := make([]string, 0, len(execErrs))
	for _, execErr := range execErrs {
		hosts = append(hosts, *execErr.instance.PublicDnsName)
		errs = append(errs, execErr.err)
	}

	if runHooks {
		var err error
		if _, err := os.Stat(FailedDeployHookScript); len(execErrs) > 0 && err == nil {
			fmt.Println(getHookMessage(FailedDeployHookScript))
			err = self.runHook(FailedDeployHookScript,
				[]string{"FAILED_HOSTS=" + strings.Join(hosts, " ")})
		} else if _, err := os.Stat(AfterDeployHookScript); err == nil {
			fmt.Println(getHookMessage(AfterDeployHookScript))
			err = self.runHook(AfterDeployHookScript, nil)
		}
		if err != nil {
			errs = append(errs, err)
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
	execArgs = append(execArgs,
		fmt.Sprintf("%s@%s", self.sshUserName(instance), *instance.PublicDnsName))
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
				self.sshUserName(instance), *instance.PublicDnsName, args[dstIndex])

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
			fmt.Fprintln(self.output, *instance.PublicDnsName)
			return nil
		}
	}
	return errors.New(instanceName + " not found")
}

/// Subtasks

func (self *Job) sshClient(i *ec2.Instance) (conn *ssh.Client, err error) {
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
		errChan <- ExecError{err: err, instance: instance}
		return
	}

	logger := self.instanceLogger(&instance)
	var stdinChannel chan []byte
	if !StdinIsTerminal() {
		stdinChannel = makeStdinChannel()
	}

	_, returnChan, err := sshRunOutLogger(conn, cmd, logger, stdinChannel)
	if err == nil {
		StartStdinRead()
		err = <-returnChan
	}
	errChan <- ExecError{err: err, instance: instance}
	return
}

func (self *Job) execInSeries(cmd string) (errs []ExecError) {
	errChan := make(chan ExecError, len(self.instances))
	go WaitForStdinStart(len(self.instances))
	errs = make([]ExecError, 0, len(self.instances))

	for _, instance := range self.instances {
		self.exec(*instance, cmd, errChan)
	}

	for _ = range self.instances {
		if err := <-errChan; err.err != nil {
			errs = append(errs, err)
		}
	}
	return
}

func (self *Job) execInParallel(cmd string) (errs []ExecError) {
	errChan := make(chan ExecError, len(self.instances))
	go WaitForStdinStart(len(self.instances))
	errs = make([]ExecError, 0, len(self.instances))

	for _, instance := range self.instances {
		go func(inst ec2.Instance) {
			self.exec(inst, cmd, errChan)
		}(*instance)
	}

	for _ = range self.instances {
		if err := <-errChan; err.err != nil {
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

func (self *Job) sshUserName(_ *ec2.Instance) (userName string) {
	// TODO: be more clever about this
	return "ubuntu"
}

func (self *Job) sshDial(i *ec2.Instance) (conn *ssh.Client, err error) {
	conn, err = sshDial(*i.PublicDnsName+":22", self.sshUserName(i))
	return
}

func (self *Job) printInstances(instances []*ec2.Instance) {
	fields := make([][]string, len(instances))
	for i, instance := range instances {
		fields[i] = []string{*instance.InstanceId, instanceLogName(instance),
			*instance.PublicDnsName}
	}
	fmt.Fprint(self.output, formatTable(fields))
}

func (self *Job) runHook(scriptPath string, environment []string) error {
	vars := make([]string, 0, len(os.Environ())+len(environment)+1)
	vars = append(vars, "ENV="+self.env)
	for _, env := range environment {
		vars = append(vars, env)
	}
	for _, env := range os.Environ() {
		vars = append(vars, env)
	}
	cmd := exec.Command("./" + scriptPath)
	cmd.Env = vars
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

func instanceLogName(i *ec2.Instance) string {
	for _, tag := range i.Tags {
		if *tag.Key == "Name" && *tag.Value != "" {
			return *tag.Value
		}
	}
	return *i.InstanceId
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

func matchCriteria(instance *ec2.Instance, criteria string) bool {
	var found bool
	for _, value := range strings.Split(criteria, "/") {
		found = false
		for _, tag := range instance.Tags {
			if strings.Contains(*tag.Value, value) {
				found = true
				break
			}
		}
		if !strings.Contains(*instance.InstanceId, value) && !strings.Contains(*instance.PrivateDnsName, value) && !strings.Contains(*instance.PublicDnsName, value) && found == false {
			return false
		}
	}
	return true
}

func getHookMessage(script string) string {
	return fmt.Sprintf("Running deploy hook %s", script)
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
