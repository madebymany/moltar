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
	"time"
)

var ErrNoInstancesFound = errors.New("No instances found; run provisioner first")
var ErrDifferentDeployRunning = errors.New("A deployment of a different version is already running")
var ErrDeployFailed = errors.New("Deploy failed")

const shWaitTailFunction = `waittail() { echo 'Waiting for zorak to receive installation request...'; while ! [ -f "$1" ]; do sleep 1; done; tail -n +0 -f "$1"; };`

type Job struct {
	region                  aws.Region
	env                     string
	cluster                 string
	project                 string
	packageName             string
	instances               []*ec2.Instance
	instanceSshClients      map[*ec2.Instance]*ssh.ClientConn
	instanceLoggers         map[*ec2.Instance]*log.Logger
	output                  io.Writer
	logger                  *log.Logger
	installVersionRev       uint64
	shouldOutputAnsiEscapes bool
}

func NewJob(awsConf AWSConf, env string, cluster string, project string, packageName string, output io.Writer, shouldOutputAnsiEscapes bool) (job *Job, err error) {
	e := ec2.New(awsConf.Auth, awsConf.Region)
	instanceFilter := ec2.NewFilter()
	instanceFilter.Add("instance-state-name", "running")
	instanceFilter.Add("tag:Environment", env)
	instanceFilter.Add("tag:Project", project)
	if cluster != "" {
		instanceFilter.Add("tag:Cluster", cluster)
	}

	if packageName != "" {
		instanceFilter.Add("tag:Packages", "*|"+packageName+"|*")
	}

	instancesResp, err := e.Instances(nil, instanceFilter)
	if err != nil {
		return
	}

	instances := make([]*ec2.Instance, 0, 20)
	for _, res := range instancesResp.Reservations {
		for _, inst := range res.Instances {
			newInst := inst
			instances = append(instances, &newInst)
		}
	}

	if len(instances) == 0 {
		return nil, ErrNoInstancesFound
	}

	logger := log.New(output, "", 0)

	return &Job{region: awsConf.Region, env: env, cluster: cluster,
		project: project, packageName: packageName, instances: instances,
		instanceSshClients: make(map[*ec2.Instance]*ssh.ClientConn),
		instanceLoggers:    make(map[*ec2.Instance]*log.Logger),
		output:             output, logger: logger,
		shouldOutputAnsiEscapes: shouldOutputAnsiEscapes}, nil
}

func (self *Job) Exec(cmd string) (errs []error) {
	errChan := make(chan error, len(self.instances))
	errs = make([]error, 0, len(self.instances))

	for _, instance := range self.instances {
		stdinChannel := makeStdinChannel()
		go func(inst ec2.Instance, stdinChannel chan []byte) {
			conn, err := self.sshClient(&inst)
			if err != nil {
				errChan <- err
				return
			}

			logger := self.instanceLogger(&inst)
			_, returnChan, err := sshRunOutLogger(conn, cmd, logger, stdinChannel)
			if err == nil {
				err = <-returnChan
			} else {
				errChan <- err
			}
			errChan <- err
		}(*instance, stdinChannel)
	}
	startStdinRead()

	for _ = range self.instances {
		if err := <-errChan; err != nil {
			errs = append(errs, err)
		}
	}
	return
}

func (self *Job) Deploy(version string) (err error) {
	if self.packageName == "" {
		return errors.New("no package name given")
	}

	deployInstance := self.instances[0]
	conn, err := self.sshClient(deployInstance)
	if err != nil {
		return
	}

	err = self.requestInstall(conn, version)
	if err != nil {
		return
	}

	logger := self.instanceLogger(deployInstance)
	term, loggerReturn, err := sshRunOutLogger(conn,
		shWaitTailFunction+" waittail "+self.logFileName(version),
		logger, nil)
	if err != nil {
		return
	}

	resp, err := etcdctl(conn,
		fmt.Sprintf("watch --after-index %d %s", self.installVersionRev,
			path.Join("/zorak/packages/installations",
				fmt.Sprintf("%d", self.installVersionRev), "cluster")))
	success := resp.Node.Value == "Success"

	select {
	case err = <-loggerReturn:
		return err
	case _ = <-time.After(time.Second * 5):
		term <- true
		err = <-loggerReturn
	}

	if !success {
		err = ErrDeployFailed
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

	initialArgs := []string{"ssh"}
	keyFile := self.keyFile()
	if keyFile != "" {
		initialArgs = append(initialArgs, []string{
			"-i", keyFile,
		}...)
	}

	finalArgs := make([]string, len(sshArgs)+len(initialArgs)+1)
	copy(finalArgs, initialArgs)
	copy(finalArgs[len(initialArgs):], sshArgs)
	finalArgs[len(finalArgs)-1] = fmt.Sprintf("%s@%s",
		self.sshUserName(instance), instance.DNSName)

	fPrintShellCommand(self.output, "", finalArgs)
	fmt.Fprintln(self.output, "")

	/* There appears to be a bug with goamz where some fds are left open, and
	 * just closing them causes a crash. If we ask all fds > 2 to close on
	 * exec, all is well.
	 */
	var rlimit syscall.Rlimit
	err = syscall.Getrlimit(syscall.RLIMIT_NOFILE, &rlimit)
	if err != nil {
		panic(err)
	}
	maxFds := int(rlimit.Cur)
	for fd := 3; fd < maxFds; fd++ {
		syscall.CloseOnExec(fd)
	}

	err = syscall.Exec(sshPath, finalArgs, os.Environ())
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

func (self *Job) requestInstall(conn *ssh.ClientConn, version string) (err error) {
	installVersionPath := path.Join("/zorak/packages/install_requests", self.packageName)

	var currentInstallVersion string
	var currentInstallVersionIndex uint64
	var subcommand string
	var resp etcdctlResponse

again:
	currentInstallVersionIndex = 0

	resp, err = etcdctl(conn, fmt.Sprintf("get '%s'", installVersionPath))
	if errIsExpectedEtcdError(err) {
		currentInstallVersion = ""
	} else if err != nil {
		return
	} else {
		currentInstallVersion = resp.Node.Value
		currentInstallVersionIndex = resp.Node.ModifiedIndex
	}

	switch currentInstallVersion {
	case "":
		self.logger.Println("Starting deployment...")
		if currentInstallVersionIndex > 0 {
			subcommand = fmt.Sprintf("set --swap-with-index %d",
				currentInstallVersionIndex)
		} else {
			subcommand = "mk"
		}
		resp, err = etcdctl(conn, fmt.Sprintf("%s %s %s", subcommand,
			installVersionPath, version))
		if err != nil && errIsExpectedEtcdError(err) {
			self.logger.Println("Conflict. Trying again...")
			goto again
		}
		self.installVersionRev = resp.Node.ModifiedIndex

	case version:
		self.logger.Println("Found running deployment for this version, picking up...")
		resp, err = etcdctl(conn, "get "+installVersionPath)
		if err != nil {
			return
		}
		self.installVersionRev = resp.Node.ModifiedIndex

	default:
		return ErrDifferentDeployRunning
	}

	return
}

/// Helpers

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

func (self *Job) keyFile() (path string) {
	fileName := self.project
	if self.packageName != "" {
		fileName += fmt.Sprintf("-%s", self.packageName)
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

func (self *Job) logFileName(version string) string {
	return fmt.Sprintf("/var/log/zorak/%s-%s-%d.log", self.packageName, version, self.installVersionRev)
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
