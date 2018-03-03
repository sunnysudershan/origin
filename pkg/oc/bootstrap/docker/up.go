package docker

import (
	"bytes"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/blang/semver"
	"github.com/docker/docker/api/types/versions"
	cliconfig "github.com/docker/docker/cli/config"
	dockerclient "github.com/docker/docker/client"
	"github.com/golang/glog"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
	"golang.org/x/net/context"

	kerrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/sets"
	kclientcmd "k8s.io/client-go/tools/clientcmd"
	kclientset "k8s.io/kubernetes/pkg/client/clientset_generated/internalclientset"
	"k8s.io/kubernetes/pkg/kubectl/cmd/templates"
	kcmdutil "k8s.io/kubernetes/pkg/kubectl/cmd/util"

	configapi "github.com/openshift/origin/pkg/cmd/server/apis/config"
	cmdutil "github.com/openshift/origin/pkg/cmd/util"
	"github.com/openshift/origin/pkg/cmd/util/variable"
	"github.com/openshift/origin/pkg/oc/bootstrap/docker/dockerhelper"
	"github.com/openshift/origin/pkg/oc/bootstrap/docker/dockermachine"
	"github.com/openshift/origin/pkg/oc/bootstrap/docker/errors"
	"github.com/openshift/origin/pkg/oc/bootstrap/docker/host"
	"github.com/openshift/origin/pkg/oc/bootstrap/docker/localcmd"
	"github.com/openshift/origin/pkg/oc/bootstrap/docker/openshift"
	"github.com/openshift/origin/pkg/oc/bootstrap/docker/run"
	"github.com/openshift/origin/pkg/oc/cli/util/clientcmd"
	osclientcmd "github.com/openshift/origin/pkg/oc/cli/util/clientcmd"
)

const (
	// CmdUpRecommendedName is the recommended command name
	CmdUpRecommendedName = "up"

	initialUser     = "developer"
	initialPassword = "developer"

	initialProjectName    = "myproject"
	initialProjectDisplay = "My Project"
	initialProjectDesc    = "Initial developer project"

	defaultRedirectClient  = "openshift-web-console"
	developmentRedirectURI = "https://localhost:9000"

	dockerAPIVersion122 = "1.22"
)

var (
	cmdUpLong = templates.LongDesc(`
		Starts an OpenShift cluster using Docker containers, provisioning a registry, router,
		initial templates, and a default project.

		This command will attempt to use an existing connection to a Docker daemon. Before running
		the command, ensure that you can execute docker commands successfully (i.e. 'docker ps').

		Optionally, the command can create a new Docker machine for OpenShift using the VirtualBox
		driver when the --create-machine argument is specified. The machine will be named 'openshift'
		by default. To name the machine differently, use the --docker-machine=NAME argument. If the
		--docker-machine=NAME argument is specified, but --create-machine is not, the command will attempt
		to find an existing docker machine with that name and start it if it's not running.

		By default, the OpenShift cluster will be setup to use a routing suffix that ends in nip.io.
		This is to allow dynamic host names to be created for routes. An alternate routing suffix
		can be specified using the --routing-suffix flag.

		A public hostname can also be specified for the server with the --public-hostname flag.`)

	cmdUpExample = templates.Examples(`
	  # Start OpenShift on a new docker machine named 'openshift'
	  %[1]s --create-machine

	  # Start OpenShift using a specific public host name
	  %[1]s --public-hostname=my.address.example.com

	  # Start OpenShift and preserve data and config between restarts
	  %[1]s --host-data-dir=/mydata --use-existing-config

	  # Use a different set of images
	  %[1]s --image="registry.example.com/origin" --version="v1.1"

	  # Specify which set of image streams to use
	  %[1]s --image-streams=centos7`)

	imageStreams = map[string]string{
		"centos7": "examples/image-streams/image-streams-centos7.json",
		"rhel7":   "examples/image-streams/image-streams-rhel7.json",
	}

	// defaultImageStreams is the default key for the above imageStreams mapping.
	// It should be set during build via -ldflags.
	defaultImageStreams string

	templateLocations = map[string]string{
		"mongodb":                     "examples/db-templates/mongodb-persistent-template.json",
		"mariadb":                     "examples/db-templates/mariadb-persistent-template.json",
		"mysql":                       "examples/db-templates/mysql-persistent-template.json",
		"postgresql":                  "examples/db-templates/postgresql-persistent-template.json",
		"cakephp quickstart":          "examples/quickstarts/cakephp-mysql-persistent.json",
		"dancer quickstart":           "examples/quickstarts/dancer-mysql-persistent.json",
		"django quickstart":           "examples/quickstarts/django-postgresql-persistent.json",
		"nodejs quickstart":           "examples/quickstarts/nodejs-mongodb-persistent.json",
		"rails quickstart":            "examples/quickstarts/rails-postgresql-persistent.json",
		"jenkins pipeline persistent": "examples/jenkins/jenkins-persistent-template.json",
		"sample pipeline":             "examples/jenkins/pipeline/samplepipeline.yaml",
	}
	// internalTemplateLocations are templates that will be registered in an internal namespace. These
	// templates are compatible with both vN and vN-1 clusters.  If they are not, they should be moved
	// into the internalCurrent and internalPrevious maps.
	internalTemplateLocations = map[string]string{
		"service catalog":                      "examples/service-catalog/service-catalog.yaml",
		"template service broker rbac":         "install/templateservicebroker/rbac-template.yaml",
		"template service broker registration": "install/service-catalog-broker-resources/template-service-broker-registration.yaml",
	}
	// internalCurrentTemplateLocations are templates that will be registered in an internal namespace.
	// These templates are for the current version of openshift (vN), for when the client version matches
	// the cluster version.
	internalCurrentTemplateLocations = map[string]string{
		"web console server template":       "install/origin-web-console/console-template.yaml",
		"template service broker apiserver": "install/templateservicebroker/apiserver-template.yaml",
	}
	// internalPreviousTemplateLocations are templates that will be registered in an internal namespace.
	// These templates are for the previous version of openshift (vN-1) to provide N-1 support for older
	// clusters from a newer client.
	internalPreviousTemplateLocations = map[string]string{
		"template service broker apiserver": "install/templateservicebroker/previous/apiserver-template.yaml",
	}

	// loggingTemplateLocations are templates that will be registered in an internal namespace
	// when logging is requested
	loggingTemplateLocations = map[string]string{
		"logging": "examples/logging/logging-deployer.yaml",
	}

	adminTemplateLocations = map[string]string{
		"prometheus":          "examples/prometheus/prometheus.yaml",
		"heapster standalone": "examples/heapster/heapster-standalone.yaml",
	}

	openshiftVersion36 = semver.MustParse("3.6.0")
	openshiftVersion37 = semver.MustParse("3.7.0")
	openshiftVersion39 = semver.MustParse("3.9.0")
)

// NewCmdUp creates a command that starts OpenShift on Docker with reasonable defaults
func NewCmdUp(name, fullName string, f *osclientcmd.Factory, out, errout io.Writer) *cobra.Command {
	config := &ClientStartConfig{
		CommonStartConfig: CommonStartConfig{
			Out:                 out,
			UsePorts:            openshift.DefaultPorts,
			PortForwarding:      defaultPortForwarding(),
			DNSPort:             openshift.DefaultDNSPort,
			checkAlternatePorts: true,
		},
	}
	cmd := &cobra.Command{
		Use:     name,
		Short:   "Start OpenShift on Docker with reasonable defaults",
		Long:    cmdUpLong,
		Example: fmt.Sprintf(cmdUpExample, fullName),
		Run: func(c *cobra.Command, args []string) {
			kcmdutil.CheckErr(config.Complete(f, c, out))
			kcmdutil.CheckErr(config.Validate(out, errout))
			kcmdutil.CheckErr(config.CommonStartConfig.Check(out))
			if err := config.Start(out); err != nil {
				fmt.Fprintf(errout, "%s\n", err.Error())
				os.Exit(1)
			}
		},
	}
	config.Bind(cmd.Flags())
	return cmd
}

// taskFunc is a function that executes a start task
type taskFunc func(io.Writer) error

// conditionFunc determines whether a task should be run on start
type conditionFunc func() bool

// task is a named task for the start process
type task struct {
	name      string
	fn        taskFunc
	condition conditionFunc
	stdOut    bool // true if task's output should go directly to stdout
}

func simpleTask(name string, fn taskFunc) task {
	return task{name: name, fn: fn}
}

func conditionalTask(name string, fn taskFunc, condition conditionFunc) task {
	return task{name: name, fn: fn, condition: condition}
}

type CommonStartConfig struct {
	ImageVersion                string
	Image                       string
	ImageStreams                string
	DockerMachine               string
	ShouldCreateDockerMachine   bool
	SkipRegistryCheck           bool
	ShouldInstallMetrics        bool
	ShouldInstallLogging        bool
	ShouldInstallServiceCatalog bool
	PortForwarding              bool

	Out   io.Writer
	Tasks []task

	HostName                 string
	LocalConfigDir           string
	UseExistingConfig        bool
	Environment              []string
	ServerLogLevel           int
	HostVolumesDir           string
	HostConfigDir            string
	WriteConfig              bool
	HostDataDir              string
	UsePorts                 []int
	DNSPort                  int
	ServerIP                 string
	AdditionalIPs            []string
	UseNsenterMount          bool
	PublicHostname           string
	RoutingSuffix            string
	HostPersistentVolumesDir string
	HTTPProxy                string
	HTTPSProxy               string
	NoProxy                  []string
	CACert                   string
	PVCount                  int

	dockerClient    dockerhelper.Interface
	dockerHelper    *dockerhelper.Helper
	hostHelper      *host.HostHelper
	openshiftHelper *openshift.Helper
	factory         *clientcmd.Factory
	originalFactory *clientcmd.Factory
	command         *cobra.Command

	usingDefaultImages         bool
	usingDefaultOpenShiftImage bool
	checkAlternatePorts        bool
	isRHDocker                 bool

	shouldInitializeData *bool
	shouldCreateUser     *bool

	containerNetworkErr chan error
}

func (c *CommonStartConfig) addTask(t task) {
	c.Tasks = append(c.Tasks, t)
}

func (config *CommonStartConfig) Bind(flags *pflag.FlagSet) {
	flags.BoolVar(&config.ShouldCreateDockerMachine, "create-machine", false, "Create a Docker machine if one doesn't exist")
	flags.StringVar(&config.DockerMachine, "docker-machine", "", "Specify the Docker machine to use")
	flags.StringVar(&config.ImageVersion, "version", "", "Specify the tag for OpenShift images")
	flags.StringVar(&config.Image, "image", variable.DefaultImagePrefix, "Specify the images to use for OpenShift")
	flags.StringVar(&config.ImageStreams, "image-streams", defaultImageStreams, "Specify which image streams to use, centos7|rhel7")
	flags.BoolVar(&config.SkipRegistryCheck, "skip-registry-check", false, "Skip Docker daemon registry check")
	flags.StringVar(&config.PublicHostname, "public-hostname", "", "Public hostname for OpenShift cluster")
	flags.StringVar(&config.RoutingSuffix, "routing-suffix", "", "Default suffix for server routes")
	flags.BoolVar(&config.UseExistingConfig, "use-existing-config", false, "Use existing configuration if present")
	flags.StringVar(&config.HostConfigDir, "host-config-dir", config.HostConfigDir, "Directory on Docker host for OpenShift configuration")
	flags.BoolVar(&config.WriteConfig, "write-config", false, "Write the configuration files into host config dir")
	flags.StringVar(&config.HostVolumesDir, "host-volumes-dir", host.DefaultVolumesDir, "Directory on Docker host for OpenShift volumes")
	flags.StringVar(&config.HostDataDir, "host-data-dir", "", "Directory on Docker host for OpenShift data. If not specified, etcd data will not be persisted on the host.")
	flags.StringVar(&config.HostPersistentVolumesDir, "host-pv-dir", host.DefaultPersistentVolumesDir, "Directory on host for OpenShift persistent volumes")
	flags.BoolVar(&config.PortForwarding, "forward-ports", config.PortForwarding, "Use Docker port-forwarding to communicate with origin container. Requires 'socat' locally.")
	flags.IntVar(&config.ServerLogLevel, "server-loglevel", 0, "Log level for OpenShift server")
	flags.StringArrayVarP(&config.Environment, "env", "e", config.Environment, "Specify a key-value pair for an environment variable to set on OpenShift container")
	flags.BoolVar(&config.ShouldInstallMetrics, "metrics", false, "Install metrics (experimental)")
	flags.BoolVar(&config.ShouldInstallLogging, "logging", false, "Install logging (experimental)")
	flags.BoolVar(&config.ShouldInstallServiceCatalog, "service-catalog", false, "Install service catalog (experimental).")
	flags.StringVar(&config.HTTPProxy, "http-proxy", "", "HTTP proxy to use for master and builds")
	flags.StringVar(&config.HTTPSProxy, "https-proxy", "", "HTTPS proxy to use for master and builds")
	flags.StringArrayVar(&config.NoProxy, "no-proxy", config.NoProxy, "List of hosts or subnets for which a proxy should not be used")
}

// Start runs the start tasks ensuring that they are executed in sequence
func (c *CommonStartConfig) Start(out io.Writer) error {
	taskPrinter := NewTaskPrinter(out)
	for _, task := range c.Tasks {
		if task.condition != nil && !task.condition() {
			continue
		}
		taskPrinter.StartTask(task.name)
		w := taskPrinter.TaskWriter()
		err := task.fn(w)
		if err != nil {
			taskPrinter.Failure(err)
			return err
		}
		taskPrinter.Success()
	}
	return nil
}

// ClientStartConfig is the configuration for the client start command
type ClientStartConfig struct {
	CommonStartConfig
}

func (config *ClientStartConfig) Bind(flags *pflag.FlagSet) {
	config.CommonStartConfig.Bind(flags)
}

func (c *CommonStartConfig) Complete(f *osclientcmd.Factory, cmd *cobra.Command, out io.Writer) error {
	c.originalFactory = f
	c.command = cmd

	// do some defaulting
	if len(c.ImageVersion) == 0 {
		c.ImageVersion = defaultImageVersion()
	}

	// do some struct initialization next

	// used for some pretty printing
	taskPrinter := NewTaskPrinter(getDetailedOut(out))

	// create a docker machine if we need one
	if c.ShouldCreateDockerMachine {
		// this default only gets set if we need to create a dockermachine.
		if len(c.DockerMachine) == 0 {
			c.DockerMachine = defaultDockerMachineName
		}

		taskPrinter.StartTask("Create Docker machine")
		fmt.Fprintf(out, "Creating docker-machine %s\n", c.DockerMachine)
		if err := CreateDockerMachine(c.DockerMachine); err != nil {
			return taskPrinter.ToError(err)
		}
		taskPrinter.Success()
	}

	// Get a Docker client.
	// If a Docker machine was specified, make sure that the machine is running.
	// Otherwise, use environment variables.
	taskPrinter.StartTask("Create Docker client")
	client, err := getDockerClient(out, c.DockerMachine, true)
	if err != nil {
		return taskPrinter.ToError(err)
	}
	c.dockerClient = client
	taskPrinter.Success()

	// TODO HOLY SIDE-EFFECTS this is setting global bool values as part of a check!
	// Check that we have the minimum Docker version available to run OpenShift
	taskPrinter.StartTask("Checking Docker version")
	if err := c.CheckDockerVersion(out); err != nil {
		return taskPrinter.ToError(err)
	}
	taskPrinter.Success()

	taskPrinter.StartTask("Checking OpenShift client")
	if err := checkOpenShiftClient(out); err != nil {
		return taskPrinter.ToError(err)
	}
	taskPrinter.Success()

	if c.PortForwarding {
		taskPrinter.StartTask("Checking prerequisites for port forwarding")
		if err := checkPortForwardingPrerequisites(out); err != nil {
			return taskPrinter.ToError(err)
		}
		taskPrinter.Success()
	}

	// Ensure that the OpenShift Docker image is available.
	// If not present, pull it.
	taskPrinter.StartTask(fmt.Sprintf("Checking for %s image", c.openshiftImage()))
	if err := c.CheckOpenShiftImage(out); err != nil {
		return taskPrinter.ToError(err)
	}
	taskPrinter.Success()

	// Ensure that the Docker daemon has the right --insecure-registry argument.
	// If not, then exit.
	if !c.SkipRegistryCheck {
		taskPrinter.StartTask("Checking Docker daemon configuration")
		if err := c.CheckDockerInsecureRegistry(out); err != nil {
			return taskPrinter.ToError(err)
		}
		taskPrinter.Success()
	}

	// TODO HOLY SIDE-EFFECTS this is setting global port values as part of a check!
	// Ensure that ports used by OpenShift are available on the host machine
	taskPrinter.StartTask("Checking for available ports")
	if err := c.CheckAvailablePorts(out); err != nil {
		return taskPrinter.ToError(err)
	}
	taskPrinter.Success()

	// TODO HOLY SIDE-EFFECTS this is setting global startup values as part of a check!
	// Check whether the Docker host has the right binaries to use Kubernetes' nsenter mounter
	// If not, use a shared volume to mount volumes on OpenShift
	taskPrinter.StartTask("Checking type of volume mount")
	if err := c.CheckNsenterMounter(out); err != nil {
		return taskPrinter.ToError(err)
	}
	taskPrinter.Success()

	// Ensure that host directories exist.
	// If not using the nsenter mounter, create a volume share on the host machine to
	// mount OpenShift volumes.
	taskPrinter.StartTask("Creating host directories")
	if err := c.EnsureHostDirectories(out); err != nil {
		return taskPrinter.ToError(err)
	}
	taskPrinter.Success()

	// TODO HOLY SIDE-EFFECTS this is setting global IP addresses
	// Determine an IP to use for OpenShift.
	// The result is that c.ServerIP will be populated with
	// the IP that will be used on the client configuration file.
	// The c.ServerIP will be set to a specific IP when:
	// 1 - DOCKER_HOST is populated with a particular tcp:// type of address
	// 2 - a docker-machine has been specified
	// 3 - 127.0.0.1 is not working and an alternate IP has been found
	// Otherwise, the default c.ServerIP will be 127.0.0.1 which is what
	// will get stored in the client's config file. The reason for this is that
	// the client config will not depend on the machine's current IP address which
	// could change over time.
	//
	// c.AdditionalIPs will be populated with additional IPs that should be
	// included in the server's certificate. These include any IPs that are currently
	// assigned to the Docker host (hostname -I)
	// Each IP is tested to ensure that it can be accessed from the current client
	taskPrinter.StartTask("Finding server IP")
	if err := c.DetermineServerIP(out); err != nil {
		return taskPrinter.ToError(err)
	}
	taskPrinter.Success()

	// this used to be done in the openshift start method, but its mutating state.
	if len(c.HTTPProxy) > 0 || len(c.HTTPSProxy) > 0 {
		c.updateNoProxy()
	}

	return nil
}

// Validate validates that required fields in StartConfig have been populated
func (c *CommonStartConfig) Validate() error {
	if c.dockerClient == nil {
		return fmt.Errorf("missing dockerClient")
	}
	return nil
}

// Check is a spot to do NON-MUTATING, preflight checks. Over time, we should try to move our non-mutating checks out of
// Complete and into Check.
func (c *CommonStartConfig) Check(out io.Writer) error {
	// used for some pretty printing
	taskPrinter := NewTaskPrinter(getDetailedOut(out))

	// Check for an OpenShift container. If one exists and is running, exit.
	// If one exists but not running, delete it.
	taskPrinter.StartTask("Checking for existing OpenShift container")
	if err := checkExistingOpenShiftContainer(c.DockerHelper(), out); err != nil {
		return taskPrinter.ToError(err)
	}
	taskPrinter.Success()

	return nil
}

// Complete initializes fields based on command parameters and execution environment
func (c *ClientStartConfig) Complete(f *osclientcmd.Factory, cmd *cobra.Command, out io.Writer) error {
	if err := c.CommonStartConfig.Complete(f, cmd, out); err != nil {
		return err
	}

	// Add default redirect URIs to an OAuthClient to enable local web-console development.
	c.addTask(conditionalTask("Adding default OAuthClient redirect URIs", c.EnsureDefaultRedirectURIs, c.ShouldInitializeData))

	// Install a registry
	c.addTask(conditionalTask("Installing registry", c.InstallRegistry, c.ShouldInitializeData))

	// Install a router
	c.addTask(conditionalTask("Installing router", c.InstallRouter, c.ShouldInitializeData))

	// Install metrics
	c.addTask(conditionalTask("Installing metrics", c.InstallMetrics, func() bool {
		return c.ShouldInstallMetrics && c.ShouldInitializeData()
	}))

	// Import default image streams
	c.addTask(conditionalTask("Importing image streams", c.ImportImageStreams, c.ShouldInitializeData))

	// Import templates
	c.addTask(conditionalTask("Importing templates", c.ImportTemplates, c.ShouldInitializeData))

	// Import internal templates
	c.addTask(conditionalTask("Importing internal templates", c.ImportInternalTemplates, c.ShouldInitializeData))

	// Import logging templates
	c.addTask(conditionalTask("Importing logging templates", c.ImportLoggingTemplates, func() bool {
		return c.ShouldInstallLogging && c.ShouldInitializeData()
	}))

	// Install logging
	c.addTask(conditionalTask("Installing logging", c.InstallLogging, func() bool {
		return c.ShouldInstallLogging && c.ShouldInitializeData()
	}))

	// Install service catalog
	c.addTask(conditionalTask("Installing service catalog", c.InstallServiceCatalog, func() bool {
		return c.ShouldInstallServiceCatalog && c.ShouldInitializeData()
	}))

	// Install template service broker if our version is high enough
	c.addTask(conditionalTask("Installing template service broker", c.InstallTemplateServiceBroker, func() bool {
		return c.ShouldInstallServiceCatalog && c.ShouldInitializeData()
	}))

	// Register the TSB w/ the SC if requested.  This is done even when reusing a config because
	// the TSB registration is not persisted.
	c.addTask(conditionalTask("Registering template service broker with service catalog", c.RegisterTemplateServiceBroker, func() bool {
		return c.ShouldInstallServiceCatalog
	}))

	// Install the web console. Do this after the template service broker is installed so that
	// the console can discover that the broker is running on startup.
	c.addTask(conditionalTask("Installing web console", c.InstallWebConsole, func() bool {
		serverVersion, _ := c.OpenShiftHelper().ServerVersion()
		return clusterVersionIsCurrent(serverVersion) && c.ShouldInitializeData()
	}))

	// Login with an initial default user
	c.addTask(conditionalTask("Login to server", c.Login, c.ShouldCreateUser))

	// Create an initial project
	c.addTask(conditionalTask(fmt.Sprintf("Creating initial project %q", initialProjectName), c.CreateProject, c.ShouldCreateUser))

	// TODO see how to restore this.  The DNS config seems different now since it is in a container.
	// Check container networking (only when loglevel > 0)
	//if glog.V(1) {
	//	c.addTask(simpleTask("Checking container networking", c.CheckContainerNetworking))
	//}

	// Display server information
	c.addTask(simpleTask("Server Information", c.ServerInfo))

	return nil
}

// Validate validates that required fields in StartConfig have been populated
func (c *ClientStartConfig) Validate(out, errout io.Writer) error {
	if err := c.CommonStartConfig.Validate(); err != nil {
		return err
	}
	cmdutil.WarnAboutCommaSeparation(errout, c.Environment, "--env")
	if len(c.Tasks) == 0 {
		return fmt.Errorf("no startup tasks to execute")
	}
	return nil
}

func getDetailedOut(out io.Writer) io.Writer {
	// When loglevel > 0, just use stdout to write all messages
	if glog.V(1) {
		return out
	} else {
		return &bytes.Buffer{}
	}
}

// Start runs the start tasks ensuring that they are executed in sequence
func (c *ClientStartConfig) Start(out io.Writer) error {
	fmt.Fprintf(out, "Starting OpenShift using %s ...\n", c.openshiftImage())

	if c.PortForwarding {
		if err := c.OpenShiftHelper().StartSocatTunnel(c.ServerIP); err != nil {
			return err
		}
	}

	if err := c.StartSelfHosted(out); err != nil {
		return err
	}
	if c.WriteConfig {
		return nil
	}
	if err := c.PostClusterStartupMutations(out); err != nil {
		return err
	}

	detailedOut := getDetailedOut(out)
	taskPrinter := NewTaskPrinter(detailedOut)
	startError := func() error {
		for _, task := range c.Tasks {
			if task.condition != nil && !task.condition() {
				continue
			}
			taskPrinter.StartTask(task.name)
			w := taskPrinter.TaskWriter()
			if task.stdOut && !bool(glog.V(1)) {
				w = io.MultiWriter(w, out)
			}
			err := task.fn(w)
			if err != nil {
				taskPrinter.Failure(err)
				return err
			}
			taskPrinter.Success()
		}
		return nil
	}()
	if startError != nil {
		if !bool(glog.V(1)) {
			fmt.Fprintf(out, "%s", detailedOut.(*bytes.Buffer).String())
		}
		return startError
	}
	if !bool(glog.V(1)) {
		c.ServerInfo(out)
	}
	return nil
}

func defaultPortForwarding() bool {
	// Defaults to true if running on Mac, with no DOCKER_HOST defined
	return runtime.GOOS == "darwin" && len(os.Getenv("DOCKER_HOST")) == 0
}

const defaultDockerMachineName = "openshift"

func defaultImageVersion() string {
	return variable.OverrideVersion.LastSemanticVersion()
}

// CreateDockerMachine will create a new Docker machine to run OpenShift
func CreateDockerMachine(dockerMachineName string) error {
	return dockermachine.NewBuilder().Name(dockerMachineName).Create()
}

// checkOpenShiftClient ensures that the client can be configured
// for the new server
func checkOpenShiftClient(out io.Writer) error {
	kubeConfig := os.Getenv("KUBECONFIG")
	if len(kubeConfig) == 0 {
		return nil
	}
	var (
		kubeConfigError error
		f               *os.File
	)
	_, err := os.Stat(kubeConfig)
	switch {
	case os.IsNotExist(err):
		err = os.MkdirAll(filepath.Dir(kubeConfig), 0755)
		if err != nil {
			kubeConfigError = fmt.Errorf("cannot make directory: %v", err)
			break
		}
		f, err = os.Create(kubeConfig)
		if err != nil {
			kubeConfigError = fmt.Errorf("cannot create file: %v", err)
			break
		}
		f.Close()
	case err == nil:
		f, err = os.OpenFile(kubeConfig, os.O_RDWR, 0644)
		if err != nil {
			kubeConfigError = fmt.Errorf("cannot open %s for write: %v", kubeConfig, err)
			break
		}
		f.Close()
	default:
		kubeConfigError = fmt.Errorf("cannot access %s: %v", kubeConfig, err)
	}
	if kubeConfigError != nil {
		return errors.ErrKubeConfigNotWriteable(kubeConfig, kubeConfigError)
	}
	return nil
}

// GetDockerClient obtains a new Docker client from the environment or
// from a Docker machine, starting it if necessary
func (c *CommonStartConfig) GetDockerClient(out io.Writer) dockerhelper.Interface {
	return c.dockerClient
}

// getDockerClient obtains a new Docker client from the environment or
// from a Docker machine, starting it if necessary and permitted
func getDockerClient(out io.Writer, dockerMachine string, canStartDockerMachine bool) (dockerhelper.Interface, error) {
	if len(dockerMachine) > 0 {
		glog.V(2).Infof("Getting client for Docker machine %q", dockerMachine)
		client, err := getDockerMachineClient(dockerMachine, out, canStartDockerMachine)
		if err != nil {
			return nil, errors.ErrNoDockerMachineClient(dockerMachine, err)
		}
		return client, nil
	}

	dockerTLSVerify := os.Getenv("DOCKER_TLS_VERIFY")
	dockerCertPath := os.Getenv("DOCKER_CERT_PATH")
	if len(dockerTLSVerify) > 0 && len(dockerCertPath) == 0 {
		dockerCertPath = cliconfig.Dir()
		os.Setenv("DOCKER_CERT_PATH", dockerCertPath)
	}

	if glog.V(4) {
		dockerHost := os.Getenv("DOCKER_HOST")
		if len(dockerHost) == 0 && len(dockerTLSVerify) == 0 && len(dockerCertPath) == 0 {
			glog.Infof("No Docker environment variables found. Will attempt default socket.")
		}
		if len(dockerHost) > 0 {
			glog.Infof("Will try Docker connection with host (DOCKER_HOST) %q", dockerHost)
		} else {
			glog.Infof("No Docker host (DOCKER_HOST) configured. Will attempt default socket.")
		}
		if len(dockerTLSVerify) > 0 {
			glog.Infof("DOCKER_TLS_VERIFY=%s", dockerTLSVerify)
		}
		if len(dockerCertPath) > 0 {
			glog.Infof("DOCKER_CERT_PATH=%s", dockerCertPath)
		}
	}
	// FIXME: Workaround for docker engine API client on OS X - sets the default to
	// the wrong DOCKER_HOST string
	if runtime.GOOS == "darwin" {
		dockerHost := os.Getenv("DOCKER_HOST")
		if len(dockerHost) == 0 {
			os.Setenv("DOCKER_HOST", "unix:///var/run/docker.sock")
		}
	}
	host := os.Getenv("DOCKER_HOST")
	if len(host) == 0 {
		host = dockerclient.DefaultDockerHost
	}
	engineAPIClient, err := dockerclient.NewEnvClient()
	if err != nil {
		return nil, errors.ErrNoDockerClient(err)
	}
	// negotiate the correct API version with the server
	ctx, fn := context.WithTimeout(context.Background(), 10*time.Second)
	defer fn()
	engineAPIClient.NegotiateAPIVersion(ctx)
	return dockerhelper.NewClient(host, engineAPIClient), nil
}

// checkExistingOpenShiftContainer checks the state of an OpenShift container.
// If one is already running, it throws an error.
// If one exists, it removes it so a new one can be created.
func checkExistingOpenShiftContainer(dockerHelper *dockerhelper.Helper, out io.Writer) error {
	container, running, err := dockerHelper.GetContainerState(openshift.OpenShiftContainer)
	if err != nil {
		return errors.NewError("unexpected error while checking OpenShift container state").WithCause(err)
	}
	if running {
		return errors.NewError("OpenShift is already running").WithSolution("To start OpenShift again, stop the current cluster:\n$ %s\n", "oc cluster down")
	}
	if container != nil {
		err = dockerHelper.RemoveContainer(openshift.OpenShiftContainer)
		if err != nil {
			return errors.NewError("cannot delete existing OpenShift container").WithCause(err)
		}
		fmt.Fprintf(out, "Deleted existing OpenShift container\n")
	}
	return nil
}

// CheckOpenShiftImage checks whether the OpenShift image exists.
// If not it tells the Docker daemon to pull it.
func (c *CommonStartConfig) CheckOpenShiftImage(out io.Writer) error {
	return c.DockerHelper().CheckAndPull(c.openshiftImage(), out)
}

// CheckDockerInsecureRegistry checks to see if the Docker daemon has an appropriate insecure registry argument set so that our services can access the registry
func (c *CommonStartConfig) CheckDockerInsecureRegistry(out io.Writer) error {
	configured, hasEntries, err := c.DockerHelper().InsecureRegistryIsConfigured()
	if err != nil {
		return err
	}
	if !configured {
		if hasEntries {
			return errors.ErrInvalidInsecureRegistryArgument()
		}
		return errors.ErrNoInsecureRegistryArgument()
	}
	return nil
}

// CheckNsenterMounter checks whether the Docker host can use the nsenter mounter from Kubernetes.
// Otherwise, a shared volume is needed in Docker
func (c *CommonStartConfig) CheckNsenterMounter(out io.Writer) error {
	useNsenterMount, err := c.HostHelper().CanUseNsenterMounter()
	if err != nil {
		return err
	}
	if useNsenterMount && c.isRHDocker {
		c.UseNsenterMount = true
		fmt.Fprintf(out, "Using nsenter mounter for OpenShift volumes\n")
	} else {
		c.UseNsenterMount = false
		fmt.Fprintf(out, "Using Docker shared volumes for OpenShift volumes\n")
	}
	return nil
}

// CheckDockerVersion checks that the appropriate Docker version is installed based on whether we are using the nsenter mounter
// or shared volumes for OpenShift
func (c *CommonStartConfig) CheckDockerVersion(out io.Writer) error {
	ver, isRHDocker, err := c.DockerHelper().APIVersion()
	if err != nil {
		glog.V(2).Infof("Failed to check Docker API version: %v", err)
		fmt.Fprintf(out, "WARNING: Cannot verify Docker version\n")
		return nil
	}
	c.isRHDocker = isRHDocker

	glog.V(5).Infof("Checking that docker API version is at least %v", dockerAPIVersion122)
	if versions.LessThan(ver, dockerAPIVersion122) {
		fmt.Fprintf(out, "WARNING: Docker version is %v, it needs to be >= %v\n", ver, dockerAPIVersion122)
	}
	return nil
}

// checkPortForwardingPrerequisites checks that socat is installed when port forwarding is enabled
// Socat needs to be installed manually on MacOS
func checkPortForwardingPrerequisites(out io.Writer) error {
	err := localcmd.New("socat").Args("-V").Run()
	if err != nil {
		glog.V(2).Infof("Error from socat command execution: %v", err)
		fmt.Fprintf(out, "WARNING: Port forwarding requires socat command line utility."+
			"Cluster public ip may not be reachable. Please make sure socat installed in your operating system \n")
	}
	return nil
}

func (c *CommonStartConfig) EnsureHostDirectories(io.Writer) error {
	return c.HostHelper().EnsureHostDirectories(!c.UseNsenterMount)
}

// EnsureDefaultRedirectURIs merges a default URL to an auth client's RedirectURIs array
func (c *ClientStartConfig) EnsureDefaultRedirectURIs(out io.Writer) error {
	factory, err := c.Factory()
	if err != nil {
		return err
	}
	oauthClient, err := factory.OpenshiftInternalOAuthClient()
	if err != nil {
		return err
	}
	webConsoleOAuth, err := oauthClient.Oauth().OAuthClients().Get(defaultRedirectClient, metav1.GetOptions{})
	if err != nil {
		if kerrors.IsNotFound(err) {
			fmt.Fprintf(out, "Unable to find OAuthClient %q\n", defaultRedirectClient)
			return nil
		}

		// announce fetch error without interrupting remaining tasks
		suggestedCmd := fmt.Sprintf("oc patch %s/%s -p '{%q:[%q]}'", "oauthclient", defaultRedirectClient, "redirectURIs", developmentRedirectURI)
		errMsg := fmt.Sprintf("Unable to fetch OAuthClient %q.\nTo manually add a development redirect URI, run %q\n", defaultRedirectClient, suggestedCmd)
		fmt.Fprintf(out, "%s\n", errMsg)
		return nil
	}

	// ensure the default redirect URI is not already present
	redirects := sets.NewString(webConsoleOAuth.RedirectURIs...)
	if redirects.Has(developmentRedirectURI) {
		return nil
	}

	webConsoleOAuth.RedirectURIs = append(webConsoleOAuth.RedirectURIs, developmentRedirectURI)

	_, err = oauthClient.Oauth().OAuthClients().Update(webConsoleOAuth)
	if err != nil {
		// announce error without interrupting remaining tasks
		suggestedCmd := fmt.Sprintf("oc patch %s/%s -p '{%q:[%q]}'", "oauthclient", defaultRedirectClient, "redirectURIs", developmentRedirectURI)
		errMsg := fmt.Sprintf("Unable to add development redirect URI to the %q OAuthClient.\nTo manually add it, run %q\n", defaultRedirectClient, suggestedCmd)
		fmt.Fprintf(out, "%s\n", errMsg)
		return nil
	}

	return nil
}

// CheckAvailablePorts ensures that ports used by OpenShift are available on the Docker host
func (c *CommonStartConfig) CheckAvailablePorts(out io.Writer) error {
	if !c.checkAlternatePorts {
		err := c.OpenShiftHelper().TestPorts(openshift.DefaultPorts)
		if err == nil {
			return nil
		}
		return errors.NewError("a port needed by OpenShift is not available").WithCause(err)
	}
	err := c.OpenShiftHelper().TestPorts(openshift.AllPorts)
	if err == nil {
		return nil
	}
	if !openshift.IsPortsNotAvailableErr(err) {
		return err
	}
	unavailable := sets.NewInt(openshift.UnavailablePorts(err)...)
	if unavailable.HasAny(openshift.BasePorts...) {
		return errors.NewError("a port needed by OpenShift is not available").WithCause(err)
	}
	if unavailable.Has(openshift.DefaultDNSPort) {
		if unavailable.Has(openshift.AlternateDNSPort) {
			return errors.NewError("a port needed by OpenShift is not available").WithCause(err)
		}
		c.DNSPort = openshift.AlternateDNSPort
		fmt.Fprintf(out, "WARNING: Binding DNS on port %d instead of 53, which may not be resolvable from all clients.\n", openshift.AlternateDNSPort)
	}

	for _, port := range openshift.RouterPorts {
		if unavailable.Has(port) {
			fmt.Fprintf(out, "WARNING: Port %d is already in use and may cause routing issues for applications.\n", port)
		}
	}
	return nil
}

// DetermineServerIP gets an appropriate IP address to communicate with the OpenShift server
func (c *CommonStartConfig) DetermineServerIP(out io.Writer) error {
	ip, err := c.determineIP(out)
	if err != nil {
		return errors.NewError("cannot determine a server IP to use").WithCause(err)
	}
	c.ServerIP = ip
	fmt.Fprintf(out, "Using %s as the server IP\n", c.ServerIP)
	c.AdditionalIPs, err = c.determineAdditionalIPs(c.ServerIP)
	if err != nil {
		return errors.NewError("cannot determine additional IPs").WithCause(err)
	}
	glog.V(4).Infof("Additional server IPs: %v", c.AdditionalIPs)
	return nil
}

// updateNoProxy will add some default values to the NO_PROXY setting if they are not present
func (c *CommonStartConfig) updateNoProxy() {
	values := []string{"127.0.0.1", c.ServerIP, "localhost", openshift.RegistryServiceIP, openshift.ServiceCatalogServiceIP, "172.30.0.0/8"}
	ipFromServer, err := c.OpenShiftHelper().ServerIP()
	if err == nil {
		values = append(values, ipFromServer)
	}
	noProxySet := sets.NewString(c.NoProxy...)
	for _, v := range values {
		if !noProxySet.Has(v) {
			noProxySet.Insert(v)
			c.NoProxy = append(c.NoProxy, v)
		}
	}
}

func (c *ClientStartConfig) PostClusterStartupMutations(out io.Writer) error {
	// Setup persistent storage
	_, kClient, err := c.Clients()
	if err != nil {
		return err
	}
	factory, err := c.Factory()
	if err != nil {
		return err
	}
	authorizationClient, err := factory.OpenshiftInternalAuthorizationClient()
	if err != nil {
		return err
	}
	securityClient, err := factory.OpenshiftInternalSecurityClient()
	if err != nil {
		return err
	}

	// Remove any duplicate nodes
	err = c.OpenShiftHelper().CheckNodes(kClient)
	if err != nil {
		return err
	}

	err = c.OpenShiftHelper().SetupPersistentStorage(authorizationClient.Authorization(), kClient, securityClient, c.HostPersistentVolumesDir, c.HostPersistentVolumesDir)
	if err != nil {
		return err
	}
	return nil
}
func (c *ClientStartConfig) CheckContainerNetworking(out io.Writer) error {
	serverIP, err := c.OpenShiftHelper().ServerIP()
	if err != nil {
		return err
	}
	var networkErr error
	if c.containerNetworkErr != nil {
		// TODO this branch should die.  Side effecty as all get out
		networkErr = <-c.containerNetworkErr
	} else {
		networkErr = c.OpenShiftHelper().TestContainerNetworking(serverIP)
	}

	if networkErr != nil {
		return errors.NewError("containers cannot communicate with the OpenShift master").
			WithDetails("The cluster was started. However, the container networking test failed.").
			WithSolution(
				fmt.Sprintf("Ensure that access to ports tcp/8443, udp/53 and udp/8053 is allowed on %s.\n"+
					"You may need to open these ports on your machine's firewall.", serverIP)).
			WithCause(networkErr)
	}
	return nil
}

func (c *CommonStartConfig) imageFormat() string {
	return fmt.Sprintf("%s-${component}:%s", c.Image, c.ImageVersion)
}

// InstallRegistry installs the OpenShift registry on the server
func (c *ClientStartConfig) InstallRegistry(out io.Writer) error {
	_, kubeClient, err := c.Clients()
	if err != nil {
		return err
	}
	f, err := c.Factory()
	if err != nil {
		return err
	}
	return c.OpenShiftHelper().InstallRegistry(kubeClient, f, c.LocalConfigDir, c.imageFormat(), c.HostPersistentVolumesDir, out, os.Stderr)
}

// InstallRouter installs a default router on the server
func (c *ClientStartConfig) InstallRouter(out io.Writer) error {
	_, kubeClient, err := c.Clients()
	if err != nil {
		return err
	}
	f, err := c.Factory()
	if err != nil {
		return err
	}
	return c.OpenShiftHelper().InstallRouter(run.NewRunHelper(c.DockerHelper()).New(), c.openshiftImage(), kubeClient, f, c.LocalConfigDir, c.imageFormat(), c.ServerIP, c.PortForwarding, out, os.Stderr)
}

// InstallWebConsole installs the OpenShift web console on the server
func (c *ClientStartConfig) InstallWebConsole(out io.Writer) error {
	f, err := c.Factory()
	if err != nil {
		return err
	}

	masterURL := c.OpenShiftHelper().Master(c.ServerIP)
	if len(c.PublicHostname) > 0 {
		masterURL = fmt.Sprintf("https://%s:8443", c.PublicHostname)
	}

	publicURL := fmt.Sprintf("%s/console/", masterURL)

	metricsURL := ""
	if c.ShouldInstallMetrics {
		metricsURL = fmt.Sprintf("https://%s/hawkular/metrics", openshift.MetricsHost(c.RoutingSuffix, c.ServerIP))
	}

	loggingURL := ""
	if c.ShouldInstallLogging {
		loggingURL = fmt.Sprintf("https://%s", openshift.LoggingHost(c.RoutingSuffix, c.ServerIP))
	}

	return c.OpenShiftHelper().InstallWebConsole(f, c.imageFormat(), c.ServerLogLevel, publicURL, masterURL, loggingURL, metricsURL)
}

// ImportImageStreams imports default image streams into the server
// TODO: Use streams compiled into oc
func (c *ClientStartConfig) ImportImageStreams(out io.Writer) error {
	imageStreamLocations := map[string]string{
		c.ImageStreams: imageStreams[c.ImageStreams],
	}
	return c.importObjects(out, openshift.OpenshiftNamespace, imageStreamLocations)
}

// ImportTemplates imports default templates into the server
// TODO: Use templates compiled into oc
func (c *ClientStartConfig) ImportTemplates(out io.Writer) error {
	if err := c.importObjects(out, openshift.OpenshiftNamespace, templateLocations); err != nil {
		return err
	}
	version, err := c.OpenShiftHelper().ServerVersion()
	if err != nil {
		return err
	}
	if shouldImportAdminTemplates(version) {
		return c.importObjects(out, "kube-system", adminTemplateLocations)
	}
	return nil
}

// ImportInternalTemplates imports internal system templates into the server
func (c *ClientStartConfig) ImportInternalTemplates(out io.Writer) error {
	if err := c.importObjects(out, openshift.OpenshiftInfraNamespace, internalTemplateLocations); err != nil {
		return err
	}
	version, err := c.OpenShiftHelper().ServerVersion()
	if err != nil {
		return err
	}
	if clusterVersionIsCurrent(version) {
		glog.V(2).Infof("Importing templates for latest version")
		if err := c.importObjects(out, openshift.OpenshiftInfraNamespace, internalCurrentTemplateLocations); err != nil {
			return err
		}
	} else {
		glog.V(2).Infof("Importing templates for previous version")
		if err := c.importObjects(out, openshift.OpenshiftInfraNamespace, internalPreviousTemplateLocations); err != nil {
			return err
		}
	}
	return nil
}

// ImportLoggingTemplates imports service catalog templates into the server
func (c *ClientStartConfig) ImportLoggingTemplates(out io.Writer) error {
	if err := c.importObjects(out, openshift.OpenshiftInfraNamespace, loggingTemplateLocations); err != nil {
		return err
	}
	return nil
}

func shouldImportAdminTemplates(v semver.Version) bool {
	return v.GTE(openshiftVersion36)
}

func useAnsible(v semver.Version) bool {
	return v.GTE(openshiftVersion36)
}

// clusterVersionIsCurrent returns whether the cluster version being
// brought up matches the client binary being used.  This needs to
// be updated each release.
func clusterVersionIsCurrent(v semver.Version) bool {
	return v.GTE(openshiftVersion39)
}

// InstallLogging will start the installation of logging components
func (c *ClientStartConfig) InstallLogging(out io.Writer) error {
	f, err := c.Factory()
	if err != nil {
		return err
	}
	publicMaster := c.PublicHostname
	if len(publicMaster) == 0 {
		publicMaster = c.ServerIP
	}
	serverVersion, _ := c.OpenShiftHelper().ServerVersion()
	if useAnsible(serverVersion) {
		return c.OpenShiftHelper().InstallLoggingViaAnsible(f,
			serverVersion,
			c.ServerIP,
			publicMaster,
			openshift.LoggingHost(c.RoutingSuffix, c.ServerIP),
			c.Image,
			c.ImageVersion,
			c.HostConfigDir,
			c.ImageStreams)
	}
	return c.OpenShiftHelper().InstallLogging(f, publicMaster, openshift.LoggingHost(c.RoutingSuffix, c.ServerIP), c.Image, c.ImageVersion)
}

// InstallMetrics will start the installation of Metrics components
func (c *ClientStartConfig) InstallMetrics(out io.Writer) error {
	f, err := c.Factory()
	if err != nil {
		return err
	}
	serverVersion, _ := c.OpenShiftHelper().ServerVersion()
	if useAnsible(serverVersion) {
		publicMaster := c.PublicHostname
		if len(publicMaster) == 0 {
			publicMaster = c.ServerIP
		}
		return c.OpenShiftHelper().InstallMetricsViaAnsible(f,
			serverVersion,
			c.ServerIP,
			publicMaster,
			openshift.MetricsHost(c.RoutingSuffix, c.ServerIP),
			c.Image,
			c.ImageVersion,
			c.HostConfigDir,
			c.ImageStreams)
	}
	return c.OpenShiftHelper().InstallMetrics(f, openshift.MetricsHost(c.RoutingSuffix, c.ServerIP), c.Image, c.ImageVersion)
}

// InstallServiceCatalog will start the installation of service catalog components
func (c *ClientStartConfig) InstallServiceCatalog(out io.Writer) error {
	f, err := c.Factory()
	if err != nil {
		return err
	}
	publicMaster := c.PublicHostname
	if len(publicMaster) == 0 {
		publicMaster = c.ServerIP
	}
	return c.OpenShiftHelper().InstallServiceCatalog(f, c.LocalConfigDir, publicMaster, openshift.CatalogHost(c.RoutingSuffix, c.ServerIP), c.imageFormat())
}

// InstallTemplateServiceBroker will start the installation of template service broker
func (c *ClientStartConfig) InstallTemplateServiceBroker(out io.Writer) error {
	f, err := c.Factory()
	if err != nil {
		return err
	}
	publicMaster := c.PublicHostname
	if len(publicMaster) == 0 {
		publicMaster = c.ServerIP
	}

	version, err := c.OpenShiftHelper().ServerVersion()
	if err != nil {
		return err
	}
	imageTemplate := fmt.Sprintf("%s-${component}:%s", c.Image, c.ImageVersion)
	if !version.GT(openshiftVersion37) {
		imageTemplate = fmt.Sprintf("%s:%s", c.Image, c.ImageVersion)
	}
	return c.OpenShiftHelper().InstallTemplateServiceBroker(f, imageTemplate, c.ServerLogLevel)
}

// RegisterTemplateServiceBroker will register the tsb with the service catalog
func (c *ClientStartConfig) RegisterTemplateServiceBroker(out io.Writer) error {
	f, err := c.Factory()
	if err != nil {
		return err
	}
	return c.OpenShiftHelper().RegisterTemplateServiceBroker(f, c.LocalConfigDir)
}

// Login logs into the new server and sets up a default user and project
func (c *ClientStartConfig) Login(out io.Writer) error {
	server := c.OpenShiftHelper().Master(c.ServerIP)
	return openshift.Login(initialUser, initialPassword, server, c.LocalConfigDir, c.originalFactory, c.command, out, out)
}

// CreateProject creates a new project for the current user
func (c *ClientStartConfig) CreateProject(out io.Writer) error {
	f, err := openshift.LoggedInUserFactory()
	if err != nil {
		return errors.NewError("cannot get logged in user client").WithCause(err)
	}
	return openshift.CreateProject(f, initialProjectName, initialProjectDisplay, initialProjectDesc, "oc", out)
}

// ServerInfo displays server information after a successful start
func (c *ClientStartConfig) ServerInfo(out io.Writer) error {
	metricsInfo := ""
	if c.ShouldInstallMetrics && c.ShouldInitializeData() {
		metricsInfo = fmt.Sprintf("The metrics service is available at:\n"+
			"    https://%s/hawkular/metrics\n\n", openshift.MetricsHost(c.RoutingSuffix, c.ServerIP))
	}
	loggingInfo := ""
	if c.ShouldInstallLogging && c.ShouldInitializeData() {
		loggingInfo = fmt.Sprintf("The kibana logging UI is available at:\n"+
			"    https://%s\n\n", openshift.LoggingHost(c.RoutingSuffix, c.ServerIP))
	}
	masterURL := c.OpenShiftHelper().Master(c.ServerIP)
	if len(c.PublicHostname) > 0 {
		masterURL = fmt.Sprintf("https://%s:8443", c.PublicHostname)
	}
	msg := fmt.Sprintf("OpenShift server started.\n\n"+
		"The server is accessible via web console at:\n"+
		"    %s\n\n%s%s", masterURL, metricsInfo, loggingInfo)

	if c.ShouldCreateUser() {
		msg += fmt.Sprintf("You are logged in as:\n"+
			"    User:     %s\n"+
			"    Password: <any value>\n\n", initialUser)
		msg += "To login as administrator:\n" +
			"    oc login -u system:admin\n\n"
	}

	msg += c.checkProxySettings()

	fmt.Fprintf(out, msg)
	return nil
}

// checkProxySettings compares proxy settings specified for cluster up
// and those on the Docker daemon and generates appropriate warnings.
func (c *ClientStartConfig) checkProxySettings() string {
	warnings := []string{}
	dockerHTTPProxy, dockerHTTPSProxy, dockerNoProxy, err := c.DockerHelper().GetDockerProxySettings()
	if err != nil {
		return "Unexpected error: " + err.Error()
	}
	// Check HTTP proxy
	if len(c.HTTPProxy) > 0 && len(dockerHTTPProxy) == 0 {
		warnings = append(warnings, "You specified an HTTP proxy for cluster up, but one is not configured for the Docker daemon")
	} else if len(c.HTTPProxy) == 0 && len(dockerHTTPProxy) > 0 {
		warnings = append(warnings, fmt.Sprintf("An HTTP proxy (%s) is configured for the Docker daemon, but you did not specify one for cluster up", dockerHTTPProxy))
	} else if c.HTTPProxy != dockerHTTPProxy {
		warnings = append(warnings, fmt.Sprintf("The HTTP proxy configured for the Docker daemon (%s) is not the same one you specified for cluster up", dockerHTTPProxy))
	}

	// Check HTTPS proxy
	if len(c.HTTPSProxy) > 0 && len(dockerHTTPSProxy) == 0 {
		warnings = append(warnings, "You specified an HTTPS proxy for cluster up, but one is not configured for the Docker daemon")
	} else if len(c.HTTPSProxy) == 0 && len(dockerHTTPSProxy) > 0 {
		warnings = append(warnings, fmt.Sprintf("An HTTPS proxy (%s) is configured for the Docker daemon, but you did not specify one for cluster up", dockerHTTPSProxy))
	} else if c.HTTPSProxy != dockerHTTPSProxy {
		warnings = append(warnings, fmt.Sprintf("The HTTPS proxy configured for the Docker daemon (%s) is not the same one you specified for cluster up", dockerHTTPSProxy))
	}

	if len(dockerHTTPProxy) > 0 || len(dockerHTTPSProxy) > 0 {
		dockerNoProxyList := strings.Split(dockerNoProxy, ",")
		dockerNoProxySet := sets.NewString(dockerNoProxyList...)
		if !dockerNoProxySet.Has(openshift.RegistryServiceIP) {
			warnings = append(warnings, fmt.Sprintf("A proxy is configured for Docker, however %[1]s is not included in its NO_PROXY list.\n"+
				"   %[1]s needs to be included in the Docker daemon's NO_PROXY environment variable so pushes to the local OpenShift registry can succeed.", openshift.RegistryServiceIP))
		}
	}

	if len(warnings) > 0 {
		buf := &bytes.Buffer{}
		for _, w := range warnings {
			fmt.Fprintf(buf, "WARNING: %s\n", w)
		}
		return buf.String()
	}
	return ""
}

// Factory returns a command factory that works with OpenShift server's admin credentials
func (c *ClientStartConfig) Factory() (*clientcmd.Factory, error) {
	if c.factory == nil {
		cfg, err := kclientcmd.LoadFromFile(filepath.Join(c.LocalConfigDir, "master", "admin.kubeconfig"))
		if err != nil {
			return nil, err
		}
		overrides := &kclientcmd.ConfigOverrides{}
		overrides.ClusterInfo.Server = fmt.Sprintf("https://%s:8443", c.ServerIP)
		defaultCfg := kclientcmd.NewDefaultClientConfig(*cfg, overrides)
		c.factory = clientcmd.NewFactory(defaultCfg)
	}
	return c.factory, nil
}

// Clients returns clients for OpenShift and Kube
// FIXME: Refactor this to KubernetesInternal() call.
func (c *ClientStartConfig) Clients() (interface{}, kclientset.Interface, error) {
	f, err := c.Factory()
	if err != nil {
		return nil, nil, err
	}
	kcset, err := f.ClientSet()
	if err != nil {
		return nil, nil, err
	}
	return nil, kcset, nil
}

// OpenShiftHelper returns a helper object to work with OpenShift on the server
func (c *CommonStartConfig) OpenShiftHelper() *openshift.Helper {
	if c.openshiftHelper == nil {
		c.openshiftHelper = openshift.NewHelper(c.DockerHelper(), c.HostHelper(), c.openshiftImage(), openshift.OpenShiftContainer, c.PublicHostname, c.RoutingSuffix)
	}
	return c.openshiftHelper
}

// HostHelper returns a helper object to check Host configuration
func (c *CommonStartConfig) HostHelper() *host.HostHelper {
	if c.hostHelper == nil {
		c.hostHelper = host.NewHostHelper(c.DockerHelper(), c.openshiftImage(), c.HostVolumesDir, c.HostConfigDir, c.HostDataDir, c.HostPersistentVolumesDir)
	}
	return c.hostHelper
}

// DockerHelper returns a helper object to work with the Docker client
func (c *CommonStartConfig) DockerHelper() *dockerhelper.Helper {
	if c.dockerHelper == nil {
		c.dockerHelper = dockerhelper.NewHelper(c.dockerClient)
	}
	return c.dockerHelper
}

func (c *ClientStartConfig) importObjects(out io.Writer, namespace string, locations map[string]string) error {
	f, err := c.Factory()
	if err != nil {
		return err
	}
	for name, location := range locations {
		glog.V(2).Infof("Importing %s from %s", name, location)
		err = openshift.ImportObjects(f, namespace, location)
		if err != nil {
			return errors.NewError("cannot import %s", name).WithCause(err).WithDetails(c.OpenShiftHelper().OriginLog())
		}
	}
	return nil
}

func (c *CommonStartConfig) openshiftImage() string {
	return fmt.Sprintf("%s:%s", c.Image, c.ImageVersion)
}

func getDockerMachineClient(machine string, out io.Writer, canStart bool) (dockerhelper.Interface, error) {
	if !dockermachine.IsRunning(machine) && canStart {
		fmt.Fprintf(out, "Starting Docker machine '%s'\n", machine)
		err := dockermachine.Start(machine)
		if err != nil {
			return nil, errors.NewError("cannot start Docker machine %q", machine).WithCause(err)
		}
		fmt.Fprintf(out, "Started Docker machine '%s'\n", machine)
	}
	return dockermachine.Client(machine)
}

func (c *CommonStartConfig) determineAdditionalIPs(ip string) ([]string, error) {
	additionalIPs := sets.NewString()
	serverIPs, err := c.OpenShiftHelper().OtherIPs(ip)
	if err != nil {
		return nil, errors.NewError("could not determine additional IPs").WithCause(err)
	}
	additionalIPs.Insert(serverIPs...)
	if c.PortForwarding {
		localIPs, err := c.localIPs()
		if err != nil {
			return nil, errors.NewError("could not determine additional local IPs").WithCause(err)
		}
		additionalIPs.Insert(localIPs...)
	}
	return additionalIPs.List(), nil
}

func (c *CommonStartConfig) localIPs() ([]string, error) {
	ips := []string{}
	devices, err := net.Interfaces()
	if err != nil {
		return nil, err
	}
	for _, dev := range devices {
		if (dev.Flags&net.FlagUp != 0) && (dev.Flags&net.FlagLoopback == 0) {
			addrs, err := dev.Addrs()
			if err != nil {
				continue
			}
			for i := range addrs {
				if ip, ok := addrs[i].(*net.IPNet); ok {
					if ip.IP.To4() != nil {
						ips = append(ips, ip.IP.String())
					}
				}
			}
		}
	}
	return ips, nil
}

func (c *CommonStartConfig) determineIP(out io.Writer) (string, error) {
	if ip := net.ParseIP(c.PublicHostname); ip != nil && !ip.IsUnspecified() {
		fmt.Fprintf(out, "Using public hostname IP %s as the host IP\n", ip)
		return ip.String(), nil
	}

	if len(c.DockerMachine) > 0 {
		// If a docker machine is specified, port forwarding will not be used
		c.PortForwarding = false
		glog.V(2).Infof("Using docker machine %q to determine server IP", c.DockerMachine)
		ip, err := dockermachine.IP(c.DockerMachine)
		if err != nil {
			return "", errors.NewError("could not determine IP address").WithCause(err).WithSolution("Ensure that docker-machine is functional.")
		}
		fmt.Fprintf(out, "Using docker-machine IP %s as the host IP\n", ip)
		return ip, nil
	}

	// If using port-forwarding, use the default loopback address
	if c.PortForwarding {
		return "127.0.0.1", nil
	}

	// Try to get the host from the DOCKER_HOST if communicating via tcp
	var err error
	ip := c.DockerHelper().HostIP()
	if ip != "" {
		glog.V(2).Infof("Testing Docker host IP (%s)", ip)
		if err = c.OpenShiftHelper().TestIP(ip); err == nil {
			return ip, nil
		}
	}
	glog.V(2).Infof("Cannot use the Docker host IP(%s): %v", ip, err)

	// If IP is not specified, try to use the loopback IP
	// This is to default to an ip-agnostic client setup
	// where the real IP of the host will not affect client operations
	if err = c.OpenShiftHelper().TestIP("127.0.0.1"); err == nil {
		return "127.0.0.1", nil
	}

	// Next, use the the --print-ip output from openshift
	ip, err = c.OpenShiftHelper().ServerIP()
	if err == nil {
		glog.V(2).Infof("Testing openshift --print-ip (%s)", ip)
		if err = c.OpenShiftHelper().TestIP(ip); err == nil {
			return ip, nil
		}
		glog.V(2).Infof("OpenShift server ip test failed: %v", err)
	}
	glog.V(2).Infof("Cannot use OpenShift IP: %v", err)

	// Next, try other IPs on Docker host
	ips, err := c.OpenShiftHelper().OtherIPs(ip)
	if err != nil {
		return "", err
	}
	for i := range ips {
		glog.V(2).Infof("Testing additional IP (%s)", ip)
		if err = c.OpenShiftHelper().TestIP(ips[i]); err == nil {
			return ip, nil
		}
		glog.V(2).Infof("OpenShift additional ip test failed: %v", err)
	}
	return "", errors.NewError("cannot determine an IP to use for your server.")
}

// ShouldInitializeData tries to determine whether we're dealing with
// an existing OpenShift data and config. It determines that data exists by checking
// for the existence of a docker-registry service.
func (c *ClientStartConfig) ShouldInitializeData() bool {
	if c.shouldInitializeData != nil {
		return *c.shouldInitializeData
	}

	result := func() bool {
		if !c.UseExistingConfig {
			return true
		}
		// For now, we determine if using existing etcd data by looking
		// for the registry service
		_, kclient, err := c.Clients()
		if err != nil {
			glog.V(2).Infof("Cannot access OpenShift master: %v", err)
			return true
		}

		if _, err = kclient.Core().Services(openshift.DefaultNamespace).Get(openshift.SvcDockerRegistry, metav1.GetOptions{}); err != nil {
			return true
		}

		// If a registry exists, then don't initialize data
		return false
	}()
	c.shouldInitializeData = &result
	return result
}

// ShouldCreateUser determines whether a user and project should
// be created. If the user provider has been modified in the config, then it should
// not attempt to create a user. Also, even if the user provider has not been
// modified, but data has been initialized, then we should also not create user.
func (c *ClientStartConfig) ShouldCreateUser() bool {
	if c.shouldCreateUser != nil {
		return *c.shouldCreateUser
	}

	result := func() bool {
		if !c.UseExistingConfig {
			return true
		}

		cfg, _, err := c.OpenShiftHelper().GetConfigFromLocalDir(c.LocalConfigDir)
		if err != nil {
			glog.V(2).Infof("error reading config: %v", err)
			return true
		}
		if cfg.OAuthConfig == nil || len(cfg.OAuthConfig.IdentityProviders) != 1 {
			return false
		}
		if _, ok := cfg.OAuthConfig.IdentityProviders[0].Provider.(*configapi.AllowAllPasswordIdentityProvider); !ok {
			return false
		}

		return c.ShouldInitializeData()
	}()

	c.shouldCreateUser = &result
	return result
}
