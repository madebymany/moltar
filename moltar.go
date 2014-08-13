package main

import (
	"bytes"
	"errors"
	"flag"
	"fmt"
	"github.com/kless/term"
	"io/ioutil"
	"log"
	"os"
	"path"
	"strings"
	"syscall"
)

var argNum = 0

var filterPackageName = flag.Bool("p", false, "filter by package name; detect it by default")
var packageName = flag.String("package", "", "package name to filter by")
var args []string

func main() {
	log.SetFlags(0)

	flag.Parse()
	args = flag.Args()

	var cluster string

	envCluster := getNextArg("environment not given")
	envClusterSplit := strings.Split(envCluster, "/")
	env := envClusterSplit[0]
	if len(envClusterSplit) > 1 {
		cluster = envClusterSplit[1]
	}

	cmd := getNextArg("command not given")

	projectName, err := detectProjectName()
	if err != nil {
		log.Fatalln(err)
	}

	if *filterPackageName && *packageName == "" {
		*packageName, err = detectPackageName()
		if err != nil {
			log.Fatalln(err)
		}
	}

	awsConf, err := getAWSConf(projectName)
	if err != nil {
		log.Fatalln(err)
	}
	job, err := NewJob(awsConf, env, cluster, projectName, *packageName,
		os.Stdout, term.IsTerminal(syscall.Stdout))
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
		hostName := getNextArg("")
		sshArgs := getRemainingArgsAsSlice("")
		err = job.Ssh(hostName, sshArgs)
	case "scp":
		if len(args) <= argNum {
			log.Fatalln("you must give at least one source file")
		}
		err = job.Scp(args[argNum:])
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

func fatalUsageError(errMsg string) {
	fmt.Fprintln(os.Stderr, "fatal: "+errMsg+"\n")
	usage()
	os.Exit(1)
}

func getNextArg(errMsg string) (val string) {
	if len(args) >= (argNum + 1) {
		val = args[argNum]
		argNum += 1
	} else if errMsg != "" {
		fatalUsageError(errMsg)
	}
	return
}

func getRemainingArgsAsString(errMsg string) (val string) {
	remainingArgs := args[argNum:]
	if len(remainingArgs) >= 1 {
		val = strings.Join(remainingArgs, " ")
	} else {
		log.Fatalln(errMsg)
	}
	return
}

func getRemainingArgsAsSlice(errMsg string) (val []string) {
	val = args[argNum:]
	if errMsg != "" && len(val) == 0 {
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

func detectPackageName() (packageName string, err error) {
	return findDotfileAndRead(".mxm-package", "Package name")
}

func formatTable(fields [][]string) (out string) {
	if len(fields) == 0 {
		return
	}
	outBuf := new(bytes.Buffer)
	numFields := len(fields[0])
	maxIndex := numFields - 1
	maxWidths := make([]int, numFields)
	for _, f := range fields {
		for i, c := range f {
			if lenc := len(c); lenc > maxWidths[i] {
				maxWidths[i] = lenc
			}
		}
	}

	for _, f := range fields {
		for i := 0; i < numFields; i++ {
			c := f[i]
			outBuf.WriteString(c)
			if i < maxIndex {
				outBuf.Write(
					bytes.Repeat([]byte(" "), maxWidths[i] - len(c) + 2))
			}
		}
		outBuf.WriteRune('\n')
	}

	out = outBuf.String()

	return
}
