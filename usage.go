package main

import (
	"fmt"
	"os"
)

const moltarUsage = `Usage:

moltar ENV CMD

Where ENV is at least one of the environment (production, staging, qa etc) and
the cluster (web, worker, search etc), separated by a slash '/'. Either or both
may be ommitted, as long as the slash remains. The slash may be ommitted if
only the environment is given. So the following are valid:

  moltar qa      # selects all instances in the 'qa' environment
  moltar qa/web  # selects all instances in the 'web' cluster in the 'qa'
                 # environment
  moltar /web    # selects all instances in a 'web' cluster over all
                 # environments
  moltar /       # selects all instances over all environments and clusters

Note that instances are still filtered by their Project tag, which must match
the project name given in the .project-name or .moltar-project file in the
current directory.

Where CMD is one of:

  exec CMD

    CMD is the command to be run on all hosts, with results reported back

  ssh NAME [ARG...]

    NAME is a string that uniquely identifies an instance. This can be part of
    the value of any tag, or its instance id, public or private DNS name. You
    can give multiple possible matching strings to disambiguate by separating
    them with slashes '/'. Give '-1' instead to just select the first
    available. Any optional ARGs are passed to the ssh command, including a
    command to be run on the instance. For example:

    moltar qa ssh 34a/53 date

    will run 'date' on the instance whose metadata has a match for both '34a'
    (in this case part of the instance ID), and '53' (part of the public DNS
    name).

    moltar qa/web ssh -1

    will open an ssh session on the first-encountered instance filtered by the
    given environment and cluster. Note this may change between calls to
    moltar, depending on the order instances are returned from the AWS API.

  scp FILE [FILE...]

    FILE is either a local file, or begins with a colon ':' and refers to a
    remote file. For example:

    moltar scp hello.jpg
    # Copies local 'hello.jpg' to the home folder of the default user on each
    # host.

    moltar scp bunnies.png :/var/www/public/
    # Copies local 'bunnies.png' to given directory on each host.

  ls

    Lists all hosts in the given environment, by Name tag and hostname.

  hostname NAME

    Gives just the public hostname of the instance identified by NAME, in the same manner as the 'ssh' command.

  deploy [PACKAGE...]

    Installs the packages detected or listed on the command-line on matching
    instances. Beyond the environment and cluster, instances must have a
    Packages tag that contains all of the packages to be installed. The
    list of packages, if one is not given on the command-line, is taken from
    the contents of a file named .package-name, .moltar-package,
    .package-names, or .moltar-packages, in that order.

`

func usage() {
	fmt.Fprint(os.Stderr, moltarUsage)
}
