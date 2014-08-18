package main

import (
	"fmt"
	"os"
)

const moltarUsage = `Usage:

moltar env cmd

Where cmd is one of:

  deploy version

    'version' is the version of the app package to be installed

  exec cmd

    'cmd' is the command to be run on all hosts, with results reported back

  ssh name

    'name' is the EC2 Name tag of the instance you want to SSH to. Give '-1' to
           just select the first available.

  scp file [file...]

    'file' is either a local file, or begins with a colon ':' and refers to a
           remote file. For example:

    moltar scp hello.jpg
    # Copies local 'hello.jpg' to the home folder of the default user on each
    # host.

    moltar scp bunnies.png :/var/www/public/
    # Copies local 'bunnies.png' to given directory on each host.

  ls

    Lists all hosts in the given environment, by Name tag and hostname.

  hostname name

    Gives just the public hostname of the instance with the given Name tag.
`

func usage() {
	fmt.Fprint(os.Stderr, moltarUsage)
}
