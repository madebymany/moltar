package main

import (
	"github.com/kless/term"
	"log"
	"os"
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

	// Region hard-coded for now, but should eventually come from
	// provisioning config
	job, err := NewJob("eu-west-1", env, projectName, appName, os.Stdout,
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
	case "ls":
		err = job.List()
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
		log.Fatalln(errMsg)
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

func detectProjectName() (projectName string, err error) {
	return "MxM", nil
}

func detectAppName() (appName string, err error) {
	return "moltar-dev", nil
}
