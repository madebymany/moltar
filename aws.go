package main

import (
	"errors"
	"github.com/vaughan0/go-ini"
	"launchpad.net/goamz/aws"
	"os"
)

type AWSConf struct {
	aws.Auth
	Region aws.Region
}

var ErrNoAccessKeyGiven = errors.New("no access key given")
var ErrUnknownRegion = errors.New("unknown region given")

func getAWSConf() (conf AWSConf, err error) {
	confFn := os.Getenv("AWS_CONFIG_FILE")
	if confFn == "" {
		confFn = os.Getenv("HOME") + "/.aws/config"
	}

	awsAuth, err := aws.EnvAuth()
	if err == nil {
		conf.Auth = awsAuth

	} else if _, err = os.Stat(confFn); os.IsNotExist(err) {
		return

	} else {
		profile := os.Getenv("AWS_DEFAULT_PROFILE")
		var section string
		if profile == "" {
			profile = "default"
			section = profile
		} else {
			section =  "profile " + profile
		}

		var iniFile ini.File
		iniFile, err = ini.LoadFile(confFn)
		if err != nil {
			return
		}

		fileConf := iniFile[section]
		if fileConf == nil {
			err = errors.New("AWS config profile '" + profile + "' does not exist")
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
