package main

import (
	"errors"
	"fmt"
	"github.com/kless/term"
	"io/ioutil"
	"log"
	"os"
	"path"
	"strings"
	"syscall"
)

var argNum = 1

func main() {
	log.SetFlags(0)

	env := getNextArg("environment not given")
	cmd := getNextArg("command not given")

	projectName, err := detectProjectName()
	if err != nil {
		log.Fatalln(err)
	}

	appName, err := detectAppName()
	if err != nil {
		log.Fatalln(err)
	}

	awsConf, err := getAWSConf(projectName)
	if err != nil {
		log.Fatalln(err)
	}
	job, err := NewJob(awsConf, env, projectName, appName, os.Stdout,
		term.IsTerminal(syscall.Stdout))
	if err != nil {
		log.Fatalln(err)
	}

	switch cmd {
	case "deploy":
		version := getNextArg("version not given")
		err = job.Deploy(version)
	case "exec":
		cmd := getRemainingArgsAsString("command not given")
		errs := job.Exec(cmd)
		if len(errs) > 0 {
			errStrings := make([]string, len(errs))
			for i, err := range errs {
				errStrings[i] = err.Error()
			}
			log.Fatalf(strings.Join(errStrings, "\n"))
		}
	case "ssh":
		hostName := getNextArg("hostname not given")
		sshArgs := getRemainingArgsAsSlice("")
		err = job.Ssh(hostName, sshArgs)
	case "scp":
		if len(os.Args) <= argNum {
			log.Fatalln("you must give at least one source file")
		}
		err = job.Scp(os.Args[argNum:])
	case "ls":
		err = job.List()
	case "hostname":
		instanceName := getNextArg("instance name not given")
		err = job.Hostname(instanceName)
	default:
		log.Fatalf("command not recognised: %s\n", cmd)
	}

	if err != nil {
		log.Fatalln(err)
	}
}

func getNextArg(errMsg string) (val string) {
	if len(os.Args) >= (argNum + 1) {
		val = os.Args[argNum]
		argNum += 1
	} else {
		fmt.Fprintln(os.Stderr, "fatal: "+errMsg+"\n")
		usage()
		os.Exit(1)
	}
	return
}

func getRemainingArgsAsString(errMsg string) (val string) {
	remainingArgs := os.Args[argNum:]
	if len(remainingArgs) >= 1 {
		val = strings.Join(remainingArgs, " ")
	} else {
		log.Fatalln(errMsg)
	}
	return
}

func getRemainingArgsAsSlice(errMsg string) (val []string) {
	val = os.Args[argNum:]
	if len(errMsg) > 0 && len(val) == 0 {
		log.Fatalln(errMsg)
	}
	return
}

func findDotfileAndRead(fn string, errName string) (value string, err error) {
	dir, err := os.Getwd()
	if err != nil {
		return
	}

	var newDir string
	for {
		if fBytes, err := ioutil.ReadFile(path.Join(dir, fn)); err == nil && len(fBytes) > 0 {
			return strings.TrimSpace(string(fBytes)), nil
		}

		newDir = path.Dir(dir)
		if dir == newDir {
			break
		}
		dir = newDir
	}

	return "", errors.New(
		fmt.Sprintf("%s not found. Please ensure your project is configured properly.", errName))
}

func detectProjectName() (projectName string, err error) {
	return findDotfileAndRead(".mxm-project", "Project name")
}

func detectAppName() (appName string, err error) {
	return findDotfileAndRead(".mxm-app", "App name")
}
