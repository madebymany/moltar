package main

import (
	"errors"
	"fmt"
	"github.com/vaughan0/go-ini"
	"launchpad.net/goamz/aws"
	"os"
	"strings"
)

type AWSConf struct {
	aws.Auth
	Region aws.Region
}

var ErrNoAccessKeyGiven = errors.New("no access key given")
var ErrUnknownRegion = errors.New("unknown region given")

func getAWSConf(projectName string) (conf AWSConf, err error) {
	confFn := os.Getenv("AWS_CONFIG_FILE")
	if confFn == "" {
		confFn = os.Getenv("HOME") + "/.aws/credentials"
		if _, err = os.Stat(confFn); os.IsNotExist(err) {
			confFn = os.Getenv("HOME") + "/.aws/config"
		}
	}

	awsAuth, err := aws.EnvAuth()
	if err == nil {
		conf.Auth = awsAuth

	} else if _, err = os.Stat(confFn); os.IsNotExist(err) {
		return

	} else {
		profiles := make([]string, 0)

		envProfile := os.Getenv("AWS_DEFAULT_PROFILE")
		if envProfile != "" {
			profiles = append(profiles, envProfile)
		}
		profiles = append(profiles, projectName)
		lowerProjectName := strings.ToLower(projectName)
		profiles = append(profiles, strings.Replace(lowerProjectName, " ", "_", -1))
		profiles = append(profiles, strings.Replace(lowerProjectName, " ", "-", -1))

		var iniFile ini.File
		iniFile, err = ini.LoadFile(confFn)
		if err != nil {
			return
		}

		var fileConf ini.Section
		for _, profile := range profiles {
			fileConf = iniFile["profile "+profile]
			if fileConf != nil {
				break
			}
		}

		if fileConf == nil {
			err = errors.New(
				fmt.Sprintf("Couldn't find a suitable AWS config profile; looked for profiles named '%s'. Please add one to your AWS config file at %s",
					strings.Join(profiles, "', '"), confFn))
			return
		}

		conf.AccessKey = fileConf["aws_access_key_id"]
		conf.SecretKey = fileConf["aws_secret_access_key"]
		conf.Region = aws.Regions[fileConf["region"]]
	}

	envRegion := os.Getenv("AWS_DEFAULT_REGION")
	if envRegion != "" {
		conf.Region = aws.Regions[envRegion]
	}

	if conf.AccessKey == "" {
		err = ErrNoAccessKeyGiven
	}

	if conf.Region.Name == "" {
		err = ErrUnknownRegion
	}

	return
}
