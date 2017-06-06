package main

import (
	"bytes"
	"flag"
	"fmt"
	"io/ioutil"
	"log"
	"os"
	"path"
	"regexp"
	"strings"
	"syscall"

	"github.com/kless/term"
)

const cmdDeploy = "deploy"
const cmdInstall = "install"

var argNum = 0

var projectName = flag.String("project", "", "project name to use for AWS credentials")
var filterPackageName = flag.Bool("p", false, "filter by package name; detect it by default")
var execInSeries = flag.Bool("s", false, "run the exec commands in series (default is parallel)")
var packageName = flag.String("package", "", "package name to filter by")
var packageVersion = flag.String("version", "", "version of packages to install")
var args []string

type dotfileNotFoundError struct {
	name string
}

func (self dotfileNotFoundError) Error() string {
	return fmt.Sprintf("%s not found. Please ensure your project is configured properly.", self.name)
}

func main() {
	log.SetFlags(0)

	flag.Parse()
	args = flag.Args()

	var cluster string
	var err error

	envCluster := getNextArg("environment not given")
	envClusterSplit := strings.Split(envCluster, "/")
	env := envClusterSplit[0]
	if len(envClusterSplit) > 1 {
		cluster = envClusterSplit[1]
	}

	cmd := getNextArg("command not given")

	if *projectName == "" {
		*projectName = os.Getenv("AWS_DEFAULT_PROFILE")
		if *projectName == "" {
			*projectName, err = detectProjectName()
			if err != nil {
				log.Fatalln(err)
			}
			if *projectName == "" {
				log.Fatalln("Please provide a profile to target")
			}
		}
	}

	var packageNames, filterPackageNames []string

	if cmd == cmdDeploy || cmd == cmdInstall {
		packageNames = getRemainingArgsAsSlice("")
		if cmd == cmdInstall && len(packageNames) == 0 {
			log.Fatalln("no packages given")
		}
	}

	if cmd == cmdDeploy {
		*filterPackageName = true
		filterPackageNames = packageNames
	}

	if *filterPackageName && (filterPackageNames == nil || len(filterPackageNames) == 0) {
		if *packageName == "" {
			filterPackageNames, err = detectPackageNames()
			if err != nil {
				log.Fatalln(err)
			}
		} else {
			filterPackageNames = []string{*packageName}
		}
	}

	if cmd == cmdDeploy {
		packageNames = filterPackageNames
	}

	awsConf, err := getAWSConf(*projectName)
	if err != nil {
		log.Fatalln(err)
	}
	job, err := NewJob(awsConf, env, cluster, *projectName, packageNames,
		filterPackageNames, os.Stdout, term.IsTerminal(syscall.Stdout))
	if err != nil {
		log.Fatalln(err)
	}

	switch cmd {
	case cmdDeploy:
		showErrorsList(job.Deploy(true, *execInSeries, *packageVersion))
	case cmdInstall:
		showErrorsList(job.Deploy(false, *execInSeries, *packageVersion))
	case "exec":
		cmd := getRemainingArgsAsString("command not given")
		showErrorsList(job.Exec(cmd, *execInSeries))
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

func showErrorsList(errs []error) {
	if len(errs) > 0 {
		errStrings := make([]string, len(errs))
		for i, err := range errs {
			errStrings[i] = err.Error()
		}
		log.Fatalf(strings.Join(errStrings, "\n"))
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
		if fBytes, err := ioutil.ReadFile(path.Join(dir, fn)); err == nil && len(fBytes) > 1 {
			return strings.TrimSpace(string(fBytes)), nil
		}

		newDir = path.Dir(dir)
		if dir == newDir {
			break
		}
		dir = newDir
	}

	return "", dotfileNotFoundError{name: errName}
}

func findDotfilesAndRead(fns []string, errName string) (value string, err error) {
	for _, fn := range fns {
		value, err = findDotfileAndRead(fn, errName)
		if err == nil {
			return value, nil
		} else {
			if _, ok := err.(dotfileNotFoundError); !ok {
				fmt.Printf("err: %#v\n", err)
				return "", err
			}
		}
	}
	return
}

func detectProjectName() (projectName string, err error) {
	return findDotfilesAndRead([]string{".project-name", ".moltar-project", ".mxm-project"}, "Project name")
}

var packageNamePattern = regexp.MustCompile(`\b[-\w]+\b`)

func detectPackageNames() (packageNames []string, err error) {
	ps, err := findDotfilesAndRead([]string{
		".package-name", ".moltar-package", ".mxm-package",
		".package-names", ".moltar-packages", ".mxm-packages",
	}, "Package name")
	if err == nil {
		return packageNamePattern.FindAllString(ps, -1), nil
	} else {
		return nil, err
	}
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
					bytes.Repeat([]byte(" "), maxWidths[i]-len(c)+2))
			}
		}
		outBuf.WriteRune('\n')
	}

	out = outBuf.String()

	return
}
