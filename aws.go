package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/credentials"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/sts"
	"github.com/go-ini/ini"
	"io/ioutil"
	"log"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

type Profile struct {
	RoleArn            string
	SourceProfile      string
	MfaSerial          string
	AwsAccessKeyId     string
	AwsSecretAccessKey string
	Region             string
	Token              string
	Name               string
}

var DefaultRegion = "eu-west-1"
var ErrNoAccessKeyGiven = errors.New("no access key given")
var ErrUnknownRegion = errors.New("unknown region given")

func getProfile(profiles []string, iniFile ini.File, hasPrefix bool) (profile Profile, err error) {
	found := false
	for _, p := range profiles {
		n := p
		if hasPrefix {
			n = "profile " + n
		}
		var section, err = iniFile.GetSection(n)
		if section != nil && err == nil {
			if section.HasKey("mfa_serial") {
				profile.MfaSerial = section.Key("mfa_serial").String()
			}
			if section.HasKey("source_profile") {
				profile.SourceProfile = section.Key("source_profile").String()
			}
			if section.HasKey("region") {
				profile.Region = section.Key("region").String()
			}
			if section.HasKey("role_arn") {
				profile.RoleArn = section.Key("role_arn").String()
			}
			if section.HasKey("aws_access_key_id") {
				profile.AwsAccessKeyId = section.Key("aws_access_key_id").String()
			}
			if section.HasKey("aws_secret_access_key") {
				profile.AwsSecretAccessKey = section.Key("aws_secret_access_key").String()
			}
			found = true
			break
		}
	}
	if found == false {
		err = errors.New(fmt.Sprintf("couldn't find any of the source profiles %s in aws credentials", strings.Join(profiles, ", ")))
		log.Fatal(err)
	}
	return
}

func getProfileKeys(profileName string) (profiles []string) {
	profiles = append(profiles, profileName)
	lowerProjectName := strings.ToLower(profileName)
	profiles = append(profiles, strings.Replace(lowerProjectName, " ", "_", -1))
	profiles = append(profiles, strings.Replace(lowerProjectName, " ", "-", -1))
	return
}

func getAWSConf(projectName string) (sess *session.Session, err error) {
	var creds *credentials.Credentials
	hasPrefix := false
	confFn := os.Getenv("AWS_CONFIG_FILE")
	if confFn == "" {
		confFn = os.Getenv("HOME") + "/.aws/credentials"
		if _, err = os.Stat(confFn); os.IsNotExist(err) {
			confFn = os.Getenv("HOME") + "/.aws/config"
			hasPrefix = true
		}
	}
	if os.Getenv("AWS_ACCESS_KEY_ID") != "" && os.Getenv("AWS_SECRET_ACCESS_KEY") != "" && (os.Getenv("AWS_DEFAULT_REGION") != "" || os.Getenv("AWS_REGION") != "") {
		creds = credentials.NewEnvCredentials()
		region := os.Getenv("AWS_DEFAULT_REGION")
		sess = session.New(&aws.Config{Credentials: creds, Region: &region})
	} else {
		var iniFile *ini.File
		iniFile, err = ini.Load(confFn)
		if err != nil {
			log.Fatalf("Failed to load AWS credentials file  %s", confFn)
		}
		profileKeys := getProfileKeys(projectName)
		profile, _ := getProfile(profileKeys, *iniFile, hasPrefix)
		profile.Name = projectName
		if profile.SourceProfile != "" {
			profileKeys = getProfileKeys(profile.SourceProfile)
			source_profile, err := getProfile(profileKeys, *iniFile, hasPrefix)
			if err == nil {
				profile.AwsAccessKeyId = source_profile.AwsAccessKeyId
				profile.AwsSecretAccessKey = source_profile.AwsSecretAccessKey
				profile.Region = source_profile.Region
			} else {
				log.Fatal(err)
			}
		}
		setProfileDefaults(&profile)
		creds = loadCachedCreds(profile)
		if creds == nil {
			if profile.RoleArn != "" {
				if profile.MfaSerial != "" {
					profile.Token = readToken()
				}
				creds = getStsCredentials(profile)
			} else {
				creds = credentials.NewStaticCredentials(profile.AwsAccessKeyId, profile.AwsSecretAccessKey, "")
				creds.Get()
			}
		}
		sess = session.New(&aws.Config{Credentials: creds, Region: &profile.Region})
	}

	return
}

func setProfileDefaults(profile *Profile) {
	if profile.Region == "" {
		profile.Region = DefaultRegion
	}
}

func getStsCredentials(profile Profile) (creds *credentials.Credentials) {
	staticCreds := credentials.NewStaticCredentials(profile.AwsAccessKeyId, profile.AwsSecretAccessKey, "")
	staticCreds.Get()
	client := sts.New(session.New(&aws.Config{Credentials: staticCreds, Region: &profile.Region}))

	sessionName := "AWS-Profile-session-" + strconv.Itoa(int(time.Now().Unix()))
	input := sts.AssumeRoleInput{
		RoleArn:         &profile.RoleArn,
		SerialNumber:    &profile.MfaSerial,
		RoleSessionName: &sessionName,
		TokenCode:       &profile.Token,
	}

	output, err := client.AssumeRole(&input)
	if err != nil {
		log.Fatal(err)
	}
	saveCachedCreds(profile, output)

	creds = credentials.NewStaticCredentials(*output.Credentials.AccessKeyId, *output.Credentials.SecretAccessKey, *output.Credentials.SessionToken)
	creds.Get()
	return
}

func readToken() (token string) {
	var err error
	for {
		fmt.Print("Enter MFA code: ")
		_, err = fmt.Scanln(&token)
		if err != nil {
			fmt.Println("There was a problem reading from stdin")
			continue
		}
		if len(token) != 6 {
			fmt.Println("Please make sure your token length is 6")
			continue
		}
		_, err = strconv.Atoi(token)
		if err != nil {
			fmt.Println("Please make sure your token is an integer")
			continue
		}
		return
	}
}

func getCachePath(profile Profile) (path string) {
	path = strings.Replace(profile.RoleArn, ":", "_", -1)
	path = strings.Replace(path, "/", "-", -1)
	path = profile.Name + "--" + path + ".json"
	usr, err := user.Current()
	if err != nil {
		log.Fatal(err)
	}
	base := filepath.Join(usr.HomeDir, ".aws/cli/cache/")
	if _, err = os.Stat(base); os.IsNotExist(err) {
		os.MkdirAll(base, 0700)
	}
	path = filepath.Join(base, path)

	return
}

func loadCachedCreds(profile Profile) (creds *credentials.Credentials) {
	path := getCachePath(profile)
	if _, err := os.Stat(path); os.IsNotExist(err) {
		return
	}

	b, err := ioutil.ReadFile(path)

	if err != nil {
		log.Fatalf("Failed to read cache path %s", path)
	}
	assumeRole := new(sts.AssumeRoleOutput)
	err = json.Unmarshal(b, &assumeRole)
	if err != nil {
		log.Fatal(err)
	}
	now := time.Now()
	if now.Unix() > assumeRole.Credentials.Expiration.Unix() {
		return
	}

	creds = credentials.NewStaticCredentials(*assumeRole.Credentials.AccessKeyId, *assumeRole.Credentials.SecretAccessKey, *assumeRole.Credentials.SessionToken)
	creds.Get()
	return
}

func saveCachedCreds(profile Profile, assumeRoleOutput *sts.AssumeRoleOutput) {
	path := getCachePath(profile)
	b, err := json.Marshal(assumeRoleOutput)
	if err != nil {
		log.Fatal(err)
	}
	err = ioutil.WriteFile(path, b, 0600)
	if err != nil {
		log.Fatalf("Failed to write to cache path %s", path)
	}
}
