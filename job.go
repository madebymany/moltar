package main

import (
	"bufio"
	"code.google.com/p/go.crypto/ssh"
	"errors"
	"fmt"
	"github.com/madebymany/doozer"
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

var ErrNoInstancesFound = errors.New("no instances found; run provisioner first")
var ErrDifferentDeployRunning = errors.New("a deployment of a different version is already running")
var ErrDeployFailed = errors.New("deploy failed")

const shWaitTailFunction = `waittail() { echo 'Waiting for zorak to receive installation request...'; while ! [ -f "$1" ]; do sleep 1; done; tail -n +0 -f "$1"; };`

type Job struct {
	region                  aws.Region
	env                     string
	project                 string
	app                     string
	packageName             string
	instances               []*ec2.Instance
	instanceSshClients      map[*ec2.Instance]*ssh.ClientConn
	instanceLoggers         map[*ec2.Instance]*log.Logger
	output                  io.Writer
	logger                  *log.Logger
	installVersionRev       int64
	shouldOutputAnsiEscapes bool
}

func NewJob(awsConf AWSConf, env string, project string, app string, output io.Writer, shouldOutputAnsiEscapes bool) (job *Job, err error) {
	e := ec2.New(awsConf.Auth, awsConf.Region)
	instanceFilter := ec2.NewFilter()
	instanceFilter.Add("instance-state-name", "running")
	instanceFilter.Add("tag:Environment", env)
	instanceFilter.Add("tag:Project", "*"+project+"*")
	instanceFilter.Add("tag:App", "*"+app+"*")

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

	packageName := os.Getenv("MOLTAR_PACKAGE_NAME")
	if packageName == "" {
		packageName = app
	}

	return &Job{region: awsConf.Region, env: env, project: project,
		app: app, packageName: packageName, instances: instances,
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
	conn, err := self.sshClient(self.instances[0])
	if err != nil {
		return
	}

	dz, err := self.doozerConn(self.instances[0])
	if err != nil {
		return
	}
	defer dz.Close()

	err = self.requestInstall(dz, version)
	if err != nil {
		return
	}
	self.logger.Printf("rev: %d\n", self.installVersionRev)

	logger := self.instanceLogger(self.instances[0])
	term, loggerReturn, err := sshRunOutLogger(conn,
		shWaitTailFunction+" waittail "+self.logFileName(version),
		logger, nil)
	if err != nil {
		return
	}

	ev, err := dz.Wait(
		path.Join("/zorak/packages", self.packageName, "cluster", version,
			fmt.Sprintf("%d", self.installVersionRev)),
		self.installVersionRev+1)
	if !ev.IsSet() {
		panic("not meant to be deleted!")
	}
	success := string(ev.Body) == "success"

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

func (self *Job) Ssh(hostName string, sshArgs []string) (err error) {
	sshPath, err := exec.LookPath("ssh")
	if err != nil {
		return
	}

	var instance *ec2.Instance
	for _, instance = range self.instances {
		if instanceLogName(instance) == hostName {
			break
		}
		instance = nil
	}

	if instance == nil {
		self.logger.Fatalf("instance '%s' not found\n", hostName)
	}

	initialArgs := []string{"ssh", "-i", self.keyFile()}
	finalArgs := make([]string, len(sshArgs)+len(initialArgs)+1)
	copy(finalArgs, initialArgs)
	copy(finalArgs[len(initialArgs):], sshArgs)
	finalArgs[len(finalArgs)-1] = fmt.Sprintf("%s@%s",
		self.sshUserName(instance), instance.DNSName)

	fPrintShellCommand(self.output, "", finalArgs)
	fmt.Fprintln(self.output, "")

	err = syscall.Exec(sshPath, finalArgs, os.Environ())
	return
}

func (self *Job) Scp(srcFiles []string, dstFile string) (err error) {
	scpPath, err := exec.LookPath("scp")
	if err != nil {
		return
	}

	dstFile = strings.Trim(dstFile, " :")
	initialArgs := []string{"-q", "-i", self.keyFile()}
	for _, fn := range srcFiles {
		initialArgs = append(initialArgs, fn)
	}
	errChan := make(chan error, len(self.instances))

	for _, instance := range self.instances {
		go func(instance *ec2.Instance) {
			var err error

			logger := self.instanceLogger(instance)
			args := make([]string, len(initialArgs)+1)
			copy(args, initialArgs)
			args[len(args)-1] = fmt.Sprintf("%s@%s:%s",
				self.sshUserName(instance), instance.DNSName, dstFile)

			fPrintShellCommand(self.output, "scp", args)

			cmd := exec.Command(scpPath, args...)
			stdout, err := cmd.StdoutPipe()
			if err != nil {
				logger.Printf("error getting stdout: %s\n", err)
				errChan <- err
				return
			}

			err = cmd.Start()
			if err != nil {
				logger.Printf("error starting scp: %s\n", err)
				errChan <- err
				return
			}

			stdoutReader := bufio.NewReader(stdout)
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
	for _, instance := range self.instances {
		fmt.Fprintf(self.output, "%s\t%s\n",
			instanceLogName(instance), instance.DNSName)
	}
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

func (self *Job) requestInstall(dz *doozer.Conn, version string) (err error) {
	installVersionPath := path.Join("/zorak/packages", self.packageName, "install-version")

	var currentInstallVersionBytes []byte
	var currentInstallVersion string
	var rev int64

again:
	currentInstallVersionBytes, rev, err = dz.Get(installVersionPath, nil)
	if err != nil {
		return
	}
	currentInstallVersion = string(currentInstallVersionBytes)

	switch currentInstallVersion {
	case "":
		self.logger.Println("Starting deployment...")

		rev, err = dz.Set(installVersionPath, rev, []byte(version))
		if err != nil {
			if derr, ok := err.(*doozer.Error); ok && derr.Err == doozer.ErrOldRev {
				self.logger.Println("Conflict. Trying again...")
				goto again
			}
		}
		self.installVersionRev = rev

	case version:
		self.logger.Println("Found running deployment for this version, picking up...")
		_, self.installVersionRev, err = dz.Stat(installVersionPath, &rev)
		if err != nil {
			return
		}

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

func (self *Job) doozerConn(i *ec2.Instance) (conn *doozer.Conn, err error) {
	ssh, err := self.sshClient(i)
	if err != nil {
		return
	}

	// Get local IP address
	ipAddr, err := sshRunOutput(ssh, `dig +short "`+i.PrivateDNSName+`"`)
	if err != nil {
		return
	}

	tcpConn, err := ssh.Dial("tcp", ipAddr+":8046")
	if err != nil {
		return
	}

	conn, err = doozer.New(tcpConn)
	return
}

func (self *Job) keyFile() (path string) {
	return fmt.Sprintf(os.ExpandEnv("${HOME}/Google Drive/%s Ops/Keys/%s-%s.pem"),
		self.project, self.app, self.env)
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

func instanceLogName(i *ec2.Instance) string {
	for _, tag := range i.Tags {
		if tag.Key == "Name" {
			return tag.Value
		}
	}
	return i.PrivateDNSName
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
