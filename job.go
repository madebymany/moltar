package main

import (
	"code.google.com/p/go.crypto/ssh"
	"errors"
	"fmt"
	"github.com/madebymany/doozer"
	"io"
	"launchpad.net/goamz/aws"
	"launchpad.net/goamz/ec2"
	"log"
	"os"
	"path"
	"time"
)

var ErrUnknownRegion = errors.New("unknown region given")
var ErrNoInstancesFound = errors.New("no instances found; run provisioner first")
var ErrDifferentDeployRunning = errors.New("a deployment of a different version is already running")
var ErrDeployFailed = errors.New("deploy failed")

const shWaitTailFunction = `waittail() { echo 'Waiting for zorak to receive installation request...'; while ! [ -f "$1" ]; do sleep 1; done; tail +0 -f "$1"; };`

type Job struct {
	regionId           string
	region             aws.Region
	env                string
	project            string
	app                string
	instances          []*ec2.Instance
	instanceSshClients map[*ec2.Instance]*ssh.ClientConn
	instanceLoggers    map[*ec2.Instance]*log.Logger
	output             io.Writer
	logger             *log.Logger
	installVersionRev  int64
}

func NewJob(regionId string, env string, project string, app string, output io.Writer) (job *Job, err error) {
	awsAuth, err := aws.EnvAuth()
	if err != nil {
		return
	}

	region := aws.Regions[regionId]
	if region.Name == "" {
		return nil, ErrUnknownRegion
	}

	e := ec2.New(awsAuth, region)
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

	return &Job{regionId: regionId, region: region, env: env, project: project,
		app: app, instances: instances,
		instanceSshClients: make(map[*ec2.Instance]*ssh.ClientConn),
		instanceLoggers:    make(map[*ec2.Instance]*log.Logger),
		output:             output, logger: logger}, nil
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
		path.Join("/zorak/packages", self.app, "cluster", version,
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

/// Subtasks

func (self *Job) requestInstall(dz *doozer.Conn, version string) (err error) {
	installVersionPath := path.Join("/zorak/packages", self.app, "install-version")

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
		logger = log.New(self.output, "\033[1m"+instanceLogName(i)+"\033[0m ", 0)
	}
	return
}

func (self *Job) doozerConn(i *ec2.Instance) (conn *doozer.Conn, err error) {
	ssh, err := self.sshClient(i)
	if err != nil {
		return
	}

	tcpConn, err := ssh.Dial("tcp", "127.0.0.1:8046")
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

func (self *Job) sshDial(i *ec2.Instance) (conn *ssh.ClientConn, err error) {
	conn, err = sshDial(i.DNSName+":22", "ubuntu", self.keyFile())
	return
}

func (self *Job) logFileName(version string) string {
	return fmt.Sprintf("/var/log/zorak/%s-%s-%d.log", self.app, version, self.installVersionRev)
}

func instanceLogName(i *ec2.Instance) string {
	for _, tag := range i.Tags {
		if tag.Key == "Name" {
			return tag.Value
		}
	}
	return i.PrivateDNSName
}
