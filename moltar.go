package main

import (
	"log"
	"os"
)

var argNum = 1

func main() {
	log.SetFlags(0)

	env := getNextArg("environment not given")
	cmd := getNextArg("command not given")
	version := getNextArg("version not given")

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
	job, err := NewJob("eu-west-1", env, projectName, appName, version, os.Stdout)
	if err != nil {
		log.Fatalln(err)
	}

	switch cmd {
	case "deploy":
		err = job.Deploy()
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

func detectProjectName() (projectName string, err error) {
	return "MxM", nil
}

func detectAppName() (appName string, err error) {
	return "moltar-dev", nil
}
