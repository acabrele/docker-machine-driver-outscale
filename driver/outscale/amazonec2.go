package outscale

import (
	"crypto/md5"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	mrand "math/rand"
	"net"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	// "github.com/aws/aws-sdk-go/aws/awserr"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/docker/machine/drivers/driverutil"
	"github.com/docker/machine/libmachine/drivers"
	"github.com/docker/machine/libmachine/log"
	"github.com/docker/machine/libmachine/mcnflag"
	"github.com/docker/machine/libmachine/mcnutils"
	"github.com/docker/machine/libmachine/ssh"
	"github.com/docker/machine/libmachine/state"
	"github.com/docker/machine/version"
)

const (
	driverName                  = "outscale"
	ipRange                     = "0.0.0.0/0"
	machineSecurityGroupName    = "rancher-nodes"
	machineTag                  = "rancher-nodes"
	defaultAmiId                = "ami-e90bc65c" //CentOS-8-2021.02.04-0 
	defaultRegion               = "us-east-2"
	defaultInstanceType         = "m5.xlarge"
	defaultRootSize             = 30
	defaultVolumeType           = "gp2"
	defaultZone                 = "us-east-2a"
	defaultSecurityGroup        = machineSecurityGroupName
	defaultSSHUser              = "outscale"
	charset                     = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ"
)

const (
	keypairNotFoundCode             = "InvalidKeyPair.NotFound"
)

var (
	dockerPort                           = 2376
	swarmPort                            = 3376
	kubeApiPort                          = 6443
	httpPort                             = 80
	httpsPort                            = 443
	nodeExporter                         = 9796
	etcdPorts                            = []int64{2379, 2380}
	clusterManagerPorts                  = []int64{6443, 6443}
	vxlanPorts                           = []int64{4789, 4789}
	flannelPorts                         = []int64{8472, 8472}
	otherKubePorts                       = []int64{10250, 10252}
	kubeProxyPorts                       = []int64{10256, 10256}
	nodePorts                            = []int64{30000, 32767}
	calicoPort                           = 179
	errorNoPrivateSSHKey                 = errors.New("using --outscale-keypair-name also requires --outscale-ssh-keypath")
	errorMissingCredentials              = errors.New("Outscale driver requires outscale credentials configured with the --outscale-access-key and --outscale-secret-key options or environment variables")
	errorNoVPCIdFound                    = errors.New("Outscale driver requires the --outscale-vpc-id option")
	errorNoSubnetsFound                  = errors.New("The desired subnet could not be located in this region. Is '--outscale-subnet-id' or OS_SUBNET_ID configured correctly?")
	errorReadingUserData                 = errors.New("unable to read --outscale-userdata file")
)

type Driver struct {
	*drivers.BaseDriver
	clientFactory         func() Ec2Client
	awsCredentialsFactory func() awsCredentials
	Id                    string
	AccessKey             string
	SecretKey             string
	SessionToken          string
	Region                string
	AMI                   string
	SSHKeyID              int
	// ExistingKey keeps track of whether the key was created by us or we used an existing one. If an existing one was used, we shouldn't delete it when the machine is deleted.
	ExistingKey      bool
	KeyName          string
	InstanceId       string
	InstanceType     string
	PrivateIPAddress string

	SecurityGroupId  string
	SecurityGroupIds []string

	SecurityGroupName  string
	SecurityGroupNames []string

	OpenPorts               []string
	Tags                    string
	ReservationId           string
	DeviceName              string
	RootSize                int64
	VolumeType              string
	IamInstanceProfile      string
	VpcId                   string
	SubnetId                string
	Zone                    string
	keyPath                 string
	PrivateIPOnly           bool
	UsePrivateIP            bool
	UseEbsOptimizedInstance bool
	SSHPrivateKeyPath       string
	RetryCount              int
	Endpoint                string
	DisableSSL              bool
	UserDataFile            string
	bdmList                 []*ec2.BlockDeviceMapping
	// Metadata Options
	HttpEndpoint string
	HttpTokens   string

	//Added for outscale
	AllocationId  string
	PublicIp      string
	AssociationId string
}

type clientFactory interface {
	build(d *Driver) Ec2Client
}

func (d *Driver) GetCreateFlags() []mcnflag.Flag {
	return []mcnflag.Flag{
		mcnflag.StringFlag{
			Name:   "outscale-access-key",
			Usage:  "Outscale Access Key",
			EnvVar: "OS_ACCESS_KEY_ID",
		},
		mcnflag.StringFlag{
			Name:   "outscale-secret-key",
			Usage:  "Outscale Secret Key",
			EnvVar: "OS_SECRET_ACCESS_KEY",
		},
		mcnflag.StringFlag{
			Name:   "outscale-session-token",
			Usage:  "Outscale Session Token",
			EnvVar: "OS_SESSION_TOKEN",
		},
		mcnflag.StringFlag{
			Name:   "outscale-ami",
			Usage:  "Outscale machine image",
			Value:  defaultAmiId,
			EnvVar: "OS_AMI",
		},
		mcnflag.StringFlag{
			Name:   "outscale-region",
			Usage:  "Outscale region",
			Value:  defaultRegion,
			EnvVar: "OS_DEFAULT_REGION",
		},
		mcnflag.StringFlag{
			Name:   "outscale-vpc-id",
			Usage:  "Outscale VPC id",
			EnvVar: "OS_VPC_ID",
		},
		mcnflag.StringFlag{
			Name:   "outscale-zone",
			Usage:  "Outscale zone for instance (i.e. a,b,c,d,e)",
			Value:  defaultZone,
			EnvVar: "OS_ZONE",
		},
		mcnflag.StringFlag{
			Name:   "outscale-subnet-id",
			Usage:  "Outscale VPC subnet id",
			EnvVar: "OS_SUBNET_ID",
		},
		mcnflag.StringSliceFlag{
			Name:   "outscale-security-group",
			Usage:  "Outscale VPC security group",
			Value:  []string{defaultSecurityGroup},
			EnvVar: "OS_SECURITY_GROUP",
		},
		mcnflag.StringSliceFlag{
			Name:  "outscale-open-port",
			Usage: "Make the specified port number accessible from the Internet",
		},
		mcnflag.StringFlag{
			Name:   "outscale-tags",
			Usage:  "Outscale Tags (e.g. key1,value1,key2,value2)",
			EnvVar: "OS_TAGS",
		},
		mcnflag.StringFlag{
			Name:   "outscale-instance-type",
			Usage:  "Outscale instance type",
			Value:  defaultInstanceType,
			EnvVar: "OS_INSTANCE_TYPE",
		},
		mcnflag.StringFlag{
			Name:   "outscale-device-name",
			Usage:  "Outscale root device name",
			EnvVar: "OS_DEVICE_NAME",
		},
		mcnflag.IntFlag{
			Name:   "outscale-root-size",
			Usage:  "Outscale root disk size (in GB)",
			Value:  defaultRootSize,
			EnvVar: "OS_ROOT_SIZE",
		},
		mcnflag.StringFlag{
			Name:   "outscale-volume-type",
			Usage:  "Outscale volume type",
			Value:  defaultVolumeType,
			EnvVar: "OS_VOLUME_TYPE",
		},
		mcnflag.StringFlag{
			Name:   "outscale-iam-instance-profile",
			Usage:  "Outscale IAM Instance Profile",
			EnvVar: "OS_INSTANCE_PROFILE",
		},
		mcnflag.StringFlag{
			Name:   "outscale-ssh-user",
			Usage:  "Set the name of the ssh user",
			Value:  defaultSSHUser,
			EnvVar: "OS_SSH_USER",
		},
		mcnflag.BoolFlag{
			Name:  "outscale-private-address-only",
			Usage: "Only use a private IP address",
		},
		mcnflag.BoolFlag{
			Name:  "outscale-use-private-address",
			Usage: "Force the usage of private IP address",
		},
		mcnflag.BoolFlag{
			Name:  "outscale-use-ebs-optimized-instance",
			Usage: "Create an EBS optimized instance",
		},
		mcnflag.StringFlag{
			Name:   "outscale-ssh-keypath",
			Usage:  "SSH Key for Instance",
			EnvVar: "OS_SSH_KEYPATH",
		},
		mcnflag.StringFlag{
			Name:   "outscale-keypair-name",
			Usage:  "Keypair to use; requires --outscale-ssh-keypath",
			EnvVar: "OS_KEYPAIR_NAME",
		},
		mcnflag.IntFlag{
			Name:  "outscale-retries",
		 	Usage: "Set retry count for recoverable failures (use -1 to disable)",
		 	Value: 5,
		 },
		mcnflag.StringFlag{
			Name:   "outscale-endpoint",
			Usage:  "Optional endpoint URL (hostname only or fully qualified URI)",
			Value:  "https://fcu.us-east-2.outscale.com",
			EnvVar: "OS_ENDPOINT",
		},
		mcnflag.StringFlag{
			Name:   "outscale-userdata",
			Usage:  "path to file with cloud-init user data",
			EnvVar: "OS_USERDATA",
		},
	}
}

func NewDriver(hostName, storePath string) *Driver {
	id := generateId()
	driver := &Driver{
		Id:                   id,
		AMI:                  defaultAmiId,
		Region:               defaultRegion,
		InstanceType:         defaultInstanceType,
		RootSize:             defaultRootSize,
		Zone:                 defaultZone,
		SecurityGroupNames:   []string{defaultSecurityGroup},
		BaseDriver: &drivers.BaseDriver{
			SSHUser:     defaultSSHUser,
			MachineName: hostName,
			StorePath:   storePath,
		},
	}

	driver.clientFactory = driver.buildClient
	driver.awsCredentialsFactory = driver.buildCredentials

	return driver
}

func (d *Driver) buildClient() Ec2Client {
	config := aws.NewConfig()
	alogger := AwsLogger()
	config = config.WithRegion(d.Region)
	config = config.WithCredentials(d.awsCredentialsFactory().Credentials())
	config = config.WithLogger(alogger)
	config = config.WithLogLevel(aws.LogDebugWithHTTPBody)
	config = config.WithMaxRetries(d.RetryCount)
	if d.Endpoint != "" {
		config = config.WithEndpoint(d.Endpoint)
		config = config.WithDisableSSL(d.DisableSSL)
	}
	return ec2.New(session.New(config))
}

func (d *Driver) buildCredentials() awsCredentials {
	return NewAWSCredentials(d.AccessKey, d.SecretKey, d.SessionToken)
}

func (d *Driver) getClient() Ec2Client {
	return d.clientFactory()
}

func (d *Driver) SetConfigFromFlags(flags drivers.DriverOptions) error {
	d.Endpoint = flags.String("outscale-endpoint")

	region, err := validateAwsRegion(flags.String("outscale-region"))
	if err != nil && d.Endpoint == "" {
		return err
	}

	image := flags.String("outscale-ami")
	if len(image) == 0 {
		image = regionDetails[region].AmiId
	}

	d.AccessKey = flags.String("outscale-access-key")
	d.SecretKey = flags.String("outscale-secret-key")
	d.SessionToken = flags.String("outscale-session-token")
	d.Region = region
	d.AMI = image
	d.InstanceType = flags.String("outscale-instance-type")
	d.VpcId = flags.String("outscale-vpc-id")
	d.SubnetId = flags.String("outscale-subnet-id")
	d.SecurityGroupNames = flags.StringSlice("outscale-security-group")
	d.Tags = flags.String("outscale-tags")
	zone := flags.String("outscale-zone")
	d.Zone = zone[:]
	d.DeviceName = flags.String("outscale-device-name")
	d.RootSize = int64(flags.Int("outscale-root-size"))
	d.VolumeType = flags.String("outscale-volume-type")
	d.IamInstanceProfile = flags.String("outscale-iam-instance-profile")
	d.SSHUser = flags.String("outscale-ssh-user")
	d.SSHPort = 22
	d.PrivateIPOnly = flags.Bool("outscale-private-address-only")
	d.UsePrivateIP = flags.Bool("outscale-use-private-address")
	d.UseEbsOptimizedInstance = flags.Bool("outscale-use-ebs-optimized-instance")
	d.SSHPrivateKeyPath = flags.String("outscale-ssh-keypath")
	d.KeyName = flags.String("outscale-keypair-name")
	d.ExistingKey = flags.String("outscale-keypair-name") != ""
	d.SetSwarmConfigFromFlags(flags)
	d.RetryCount = flags.Int("outscale-retries")
	d.OpenPorts = flags.StringSlice("outscale-open-port")
	d.UserDataFile = flags.String("outscale-userdata")
	d.DisableSSL = false

	if d.KeyName != "" && d.SSHPrivateKeyPath == "" {
	 	return errorNoPrivateSSHKey
	}

	_, err = d.awsCredentialsFactory().Credentials().Get()
	if err != nil {
		return errorMissingCredentials
	}

	if d.VpcId == "" {
		d.VpcId, err = d.getDefaultVPCId()
		if err != nil {
			log.Warnf("Couldn't determine your account Default VPC ID : %q", err)
		}
	}

	if d.SubnetId == "" && d.VpcId == "" {
		return errorNoVPCIdFound
	}

	if d.SubnetId != "" && d.VpcId != "" {
		subnetFilter := []*ec2.Filter{
			{
				Name:   aws.String("subnet-id"),
				Values: []*string{&d.SubnetId},
			},
		}

		subnets, err := d.getClient().DescribeSubnets(&ec2.DescribeSubnetsInput{
			Filters: subnetFilter,
		})
		if err != nil {
			return err
		}

		if subnets == nil || len(subnets.Subnets) == 0 {
			return errorNoSubnetsFound
		}

		if *subnets.Subnets[0].VpcId != d.VpcId {
			return fmt.Errorf("SubnetId: %s does not belong to VpcId: %s", d.SubnetId, d.VpcId)
		}
	}

	if d.isSwarmMaster() {
		u, err := url.Parse(d.SwarmHost)
		if err != nil {
			return fmt.Errorf("error parsing swarm host: %s", err)
		}

		parts := strings.Split(u.Host, ":")
		port, err := strconv.Atoi(parts[1])
		if err != nil {
			return err
		}

		swarmPort = port
	}

	return nil
}

// DriverName returns the name of the driver
func (d *Driver) DriverName() string {
	return driverName
}

func (d *Driver) checkSubnet() error {
	regionZone := d.getRegionZone()
	if d.SubnetId == "" {
		filters := []*ec2.Filter{
			{
				Name:   aws.String("availability-zone"),
				Values: []*string{&regionZone},
			},
			{
				Name:   aws.String("vpc-id"),
				Values: []*string{&d.VpcId},
			},
		}

		subnets, err := d.getClient().DescribeSubnets(&ec2.DescribeSubnetsInput{
			Filters: filters,
		})
		if err != nil {
			return err
		}

		if len(subnets.Subnets) == 0 {
			return fmt.Errorf("unable to find a subnet in the zone: %s", regionZone)
		}

		d.SubnetId = *subnets.Subnets[0].SubnetId

		// try to find default
		if len(subnets.Subnets) > 1 {
			for _, subnet := range subnets.Subnets {
				if subnet.DefaultForAz != nil && *subnet.DefaultForAz {
					d.SubnetId = *subnet.SubnetId
					break
				}
			}
		}
	}

	return nil
}

func (d *Driver) checkAMI() error {
	// Check if image exists
	images, err := d.getClient().DescribeImages(&ec2.DescribeImagesInput{
		ImageIds: []*string{&d.AMI},
	})
	if err != nil {
		return err
	}
	if len(images.Images) == 0 {
		return fmt.Errorf("AMI %s not found on region %s", d.AMI, d.getRegionZone())
	}

	// Select the right device name, if not provided
	if d.DeviceName == "" {
		d.DeviceName = *images.Images[0].RootDeviceName
	}

	//store bdm list && update size and encryption settings
	d.bdmList = images.Images[0].BlockDeviceMappings

	return nil
}

func (d *Driver) PreCreateCheck() error {
	if err := d.checkSubnet(); err != nil {
		return err
	}

	if err := d.checkAMI(); err != nil {
		return err
	}

	return nil
}

func (d *Driver) instanceIpAvailable() bool {
	ip, err := d.GetIP()
	if err != nil {
		log.Debug(err)
	}
	if ip != "" {
		d.IPAddress = ip
		log.Debugf("Got the IP Address, it's %q", d.IPAddress)
		return true
	}
	return false
}

func makePointerSlice(stackSlice []string) []*string {
	pointerSlice := []*string{}
	for i := range stackSlice {
		pointerSlice = append(pointerSlice, &stackSlice[i])
	}
	return pointerSlice
}

// Support migrating single string Driver fields to slices.
func migrateStringToSlice(value string, values []string) (result []string) {
	if value != "" {
		result = append(result, value)
	}
	result = append(result, values...)
	return
}

func (d *Driver) securityGroupNames() (ids []string) {
	return migrateStringToSlice(d.SecurityGroupName, d.SecurityGroupNames)
}

func (d *Driver) securityGroupIds() (ids []string) {
	return migrateStringToSlice(d.SecurityGroupId, d.SecurityGroupIds)
}

func (d *Driver) Base64UserData() (userdata string, err error) {
	if d.UserDataFile != "" {
		buf, ioerr := ioutil.ReadFile(d.UserDataFile)
		if ioerr != nil {
			log.Warnf("failed to read user data file %q: %s", d.UserDataFile, ioerr)
			err = errorReadingUserData
			return
		}
		userdata = base64.StdEncoding.EncodeToString(buf)
	}
	return
}

func (d *Driver) Create() error {
	// PreCreateCheck has already been called

	if err := d.innerCreate(); err != nil {
		// cleanup partially created resources
		d.Remove()
		return err
	}

	return nil
}

func (d *Driver) innerCreate() error {
	log.Infof("Launching instance...")

	if err := d.createKeyPair(); err != nil {
		return fmt.Errorf("unable to create key pair: %s", err)
	}

	if err := d.configureSecurityGroups(d.securityGroupNames()); err != nil {
		return err
	}

	var userdata string
	if b64, err := d.Base64UserData(); err != nil {
		return err
	} else {
		userdata = b64
	}

	bdmList := d.updateBDMList()

	netSpecs := []*ec2.InstanceNetworkInterfaceSpecification{{
		DeviceIndex:              aws.Int64(0), // eth0
		Groups:                   makePointerSlice(d.securityGroupIds()),
		SubnetId:                 &d.SubnetId,
		AssociatePublicIpAddress: aws.Bool(!d.PrivateIPOnly),
	}}

	regionZone := d.getRegionZone()
	log.Debugf("launching instance in subnet %s", d.SubnetId)

	var instance *ec2.Instance
		inst, err := d.getClient().RunInstances(&ec2.RunInstancesInput{
			ImageId:  &d.AMI,
			MinCount: aws.Int64(1),
			MaxCount: aws.Int64(1),
			Placement: &ec2.Placement{
				AvailabilityZone: &regionZone,
			},
			KeyName:           &d.KeyName,
			InstanceType:      &d.InstanceType,
			NetworkInterfaces: netSpecs,
			IamInstanceProfile: &ec2.IamInstanceProfileSpecification{
				Name: &d.IamInstanceProfile,
			},
			EbsOptimized:        &d.UseEbsOptimizedInstance,
			BlockDeviceMappings: bdmList,
			UserData:            &userdata,
		})

		if err != nil {
			return fmt.Errorf("Error launching instance: %s", err)
		}
		instance = inst.Instances[0]
	// }

	d.InstanceId = *instance.InstanceId

	//Outscale does not provision an Extenal IP automatically so need to do it
	//here before the IP can be discovered

	d.waitForInstance()

	log.Debug("Allocating External IP Address")

	eip, err := d.getClient().AllocateAddress(&ec2.AllocateAddressInput{
		Domain: aws.String("vpc"),
	})

	if err != nil {
		return fmt.Errorf("Error allocating external IP: %s", err)
	}
	d.AllocationId = *eip.AllocationId
	d.PublicIp = *eip.PublicIp

	log.Debug("Associating External IP Address")
	_, err = d.getClient().AssociateAddress(&ec2.AssociateAddressInput{
		AllocationId: aws.String(d.AllocationId),
		InstanceId:   aws.String(d.InstanceId),
		PublicIp:     aws.String(d.PublicIp),
	})
	if err != nil {
		return fmt.Errorf("Error associating external IP: %s", err)
	} else {
		log.Debug("waiting for ip address to become available")
		if err := mcnutils.WaitFor(d.instanceIpAvailable); err != nil {
			return err
		}
	}

	//End outscale specifics

	if instance.PrivateIpAddress != nil {
		d.PrivateIPAddress = *instance.PrivateIpAddress
	}

	//d.waitForInstance()

	if d.HttpEndpoint != "" || d.HttpTokens != "" {
		_, err := d.getClient().ModifyInstanceMetadataOptions(&ec2.ModifyInstanceMetadataOptionsInput{
			InstanceId:   aws.String(d.InstanceId),
			HttpEndpoint: aws.String(d.HttpEndpoint),
			HttpTokens:   aws.String(d.HttpTokens),
		})
		if err != nil {
			return fmt.Errorf("Error modifying instance metadata options for instance: %s", err)
		}
	}

	log.Debugf("created instance ID %s, IP address %s, Private IP address %s",
		d.InstanceId,
		d.IPAddress,
		d.PrivateIPAddress,
	)

	log.Debug("Settings tags for instance")
	err = d.configureTags(d.Tags)

	if err != nil {
		return fmt.Errorf("Unable to tag instance %s: %s", d.InstanceId, err)
	}

	return nil
}

func (d *Driver) GetURL() (string, error) {
	if err := drivers.MustBeRunning(d); err != nil {
		return "", err
	}

	ip, err := d.GetIP()
	if err != nil {
		return "", err
	}
	if ip == "" {
		return "", nil
	}

	return fmt.Sprintf("tcp://%s", net.JoinHostPort(ip, strconv.Itoa(dockerPort))), nil
}

func (d *Driver) GetIP() (string, error) {
	inst, err := d.getInstance()
	if err != nil {
		return "", err
	}

	if d.PrivateIPOnly {
		if inst.PrivateIpAddress == nil {
			return "", fmt.Errorf("No private IP for instance %v", *inst.InstanceId)
		}
		return *inst.PrivateIpAddress, nil
	}

	if d.UsePrivateIP {
		if inst.PrivateIpAddress == nil {
			return "", fmt.Errorf("No private IP for instance %v", *inst.InstanceId)
		}
		return *inst.PrivateIpAddress, nil
	}

	if inst.PublicIpAddress == nil {
		return "", fmt.Errorf("No IP for instance %v", *inst.InstanceId)
	}
	return *inst.PublicIpAddress, nil
}

func (d *Driver) GetState() (state.State, error) {
	inst, err := d.getInstance()
	if err != nil {
		return state.Error, err
	}
	switch *inst.State.Name {
	case ec2.InstanceStateNamePending:
		return state.Starting, nil
	case ec2.InstanceStateNameRunning:
		return state.Running, nil
	case ec2.InstanceStateNameStopping:
		return state.Stopping, nil
	case ec2.InstanceStateNameShuttingDown:
		return state.Stopping, nil
	case ec2.InstanceStateNameStopped:
		return state.Stopped, nil
	case ec2.InstanceStateNameTerminated:
		return state.Error, nil
	default:
		log.Warnf("unrecognized instance state: %v", *inst.State.Name)
		return state.Error, nil
	}
}

func (d *Driver) GetSSHHostname() (string, error) {
	return d.GetIP()
}

func (d *Driver) GetSSHUsername() string {
	if d.SSHUser == "" {
		d.SSHUser = defaultSSHUser
	}

	return d.SSHUser
}

func (d *Driver) Start() error {
	_, err := d.getClient().StartInstances(&ec2.StartInstancesInput{
		InstanceIds: []*string{&d.InstanceId},
	})
	if err != nil {
		return err
	}

	return d.waitForInstance()
}

func (d *Driver) Stop() error {
	_, err := d.getClient().StopInstances(&ec2.StopInstancesInput{
		InstanceIds: []*string{&d.InstanceId},
		Force:       aws.Bool(false),
	})
	return err
}

func (d *Driver) Restart() error {
	_, err := d.getClient().RebootInstances(&ec2.RebootInstancesInput{
		InstanceIds: []*string{&d.InstanceId},
	})
	return err
}

func (d *Driver) Kill() error {
	_, err := d.getClient().StopInstances(&ec2.StopInstancesInput{
		InstanceIds: []*string{&d.InstanceId},
		Force:       aws.Bool(true),
	})
	return err
}

func (d *Driver) Remove() error {
	multierr := mcnutils.MultiError{
		Errs: []error{},
	}

	if err := d.terminate(); err != nil {
		multierr.Errs = append(multierr.Errs, err)
	}

	if !d.ExistingKey {
		if err := d.deleteKeyPair(); err != nil {
			multierr.Errs = append(multierr.Errs, err)
		}
	}

	if len(multierr.Errs) == 0 {
		return nil
	}

	return multierr
}

func (d *Driver) getInstance() (*ec2.Instance, error) {
	instances, err := d.getClient().DescribeInstances(&ec2.DescribeInstancesInput{
		InstanceIds: []*string{&d.InstanceId},
	})
	if err != nil {
		return nil, err
	}
	return instances.Reservations[0].Instances[0], nil
}

func (d *Driver) instanceIsRunning() bool {
	st, err := d.GetState()
	if err != nil {
		log.Debug(err)
	}
	if st == state.Running {
		return true
	}
	return false
}

func (d *Driver) waitForInstance() error {
	if err := mcnutils.WaitFor(d.instanceIsRunning); err != nil {
		return err
	}

	return nil
}

func (d *Driver) createKeyPair() error {
	keyPath := ""

	if d.SSHPrivateKeyPath == "" {
		log.Debugf("Creating New SSH Key")
		if err := ssh.GenerateSSHKey(d.GetSSHKeyPath()); err != nil {
			return err
		}
		keyPath = d.GetSSHKeyPath()
	} else {
		log.Debugf("Using SSHPrivateKeyPath: %s", d.SSHPrivateKeyPath)
		if err := mcnutils.CopyFile(d.SSHPrivateKeyPath, d.GetSSHKeyPath()); err != nil {
			return err
		}
		if d.KeyName != "" {
			log.Debugf("Using existing EC2 key pair: %s", d.KeyName)
			return nil
		}
		if err := mcnutils.CopyFile(d.SSHPrivateKeyPath+".pub", d.GetSSHKeyPath()+".pub"); err != nil {
			return err
		}
		keyPath = d.SSHPrivateKeyPath
	}

	publicKey, err := ioutil.ReadFile(keyPath + ".pub")
	if err != nil {
		return err
	}

	r := mrand.New(mrand.NewSource(time.Now().UnixNano()))
	b := make([]byte, 5)
	for i := range b {
		b[i] = charset[r.Intn(len(charset))]
	}
	keyName := d.MachineName + "-" + string(b)

	log.Debugf("creating key pair: %s", keyName)
	_, err = d.getClient().ImportKeyPair(&ec2.ImportKeyPairInput{
		KeyName:           &keyName,
		PublicKeyMaterial: publicKey,
	})
	if err != nil {
		return err
	}
	d.KeyName = keyName
	return nil
}

func (d *Driver) terminate() error {
	if d.InstanceId == "" {
		log.Warn("Missing instance ID, this is likely due to a failure during machine creation")
		return nil
	}

	log.Debugf("terminating instance: %s", d.InstanceId)
	_, err := d.getClient().TerminateInstances(&ec2.TerminateInstancesInput{
		InstanceIds: []*string{&d.InstanceId},
	})

	if err != nil {
		if strings.HasPrefix(err.Error(), "unknown instance") ||
			strings.HasPrefix(err.Error(), "InvalidInstanceID.NotFound") {
			log.Warn("Remote instance does not exist, proceeding with removing local reference")
			return nil
		}

		return fmt.Errorf("unable to terminate instance: %s", err)
	}
	return nil
}

func (d *Driver) isSwarmMaster() bool {
	return d.SwarmMaster
}

func (d *Driver) securityGroupAvailableFunc(id string) func() bool {
	return func() bool {

		securityGroup, err := d.getClient().DescribeSecurityGroups(&ec2.DescribeSecurityGroupsInput{
			GroupIds: []*string{&id},
		})
		if err == nil && len(securityGroup.SecurityGroups) > 0 {
			return true
		} else if err == nil {
			log.Debugf("No security group with id %v found", id)
			return false
		}
		log.Debug(err)
		return false
	}
}

func (d *Driver) configureTags(tagGroups string) error {

	tags := []*ec2.Tag{}
	tags = append(tags, &ec2.Tag{
		Key:   aws.String("Name"),
		Value: &d.MachineName,
	})

	//Added for outscale, where the instance requires tagging to be used with the cloud provider for outscale 
	//This assumes the hostname (which populates MachineName) uses the format of clustername-
	ClusterName := d.MachineName[:strings.IndexByte(d.MachineName, '-')]
	tags = append(tags, &ec2.Tag{
		Key:   aws.String("OscK8sClusterID/" + ClusterName),
		Value: aws.String("owned"),
	}, &ec2.Tag{
		Key:   aws.String("OscK8sNodeName"),
		Value: &d.MachineName,
	})

	if tagGroups != "" {
		t := strings.Split(tagGroups, ",")
		if len(t) > 0 && len(t)%2 != 0 {
			log.Warnf("Tags are not key value in pairs. %d elements found", len(t))
		}
		for i := 0; i < len(t)-1; i += 2 {
			tags = append(tags, &ec2.Tag{
				Key:   &t[i],
				Value: &t[i+1],
			})
		}
	}

	_, err := d.getClient().CreateTags(&ec2.CreateTagsInput{
		Resources: []*string{&d.InstanceId},
		Tags:      tags,
	})

	if err != nil {
		return err
	}

	return nil
}

func (d *Driver) configureSecurityGroups(groupNames []string) error {
	if len(groupNames) == 0 {
		log.Debugf("no security groups to configure in %s", d.VpcId)
		return nil
	}

	log.Debugf("configuring security groups in %s", d.VpcId)
	version := version.Version

	filters := []*ec2.Filter{
		{
			Name:   aws.String("group-name"),
			Values: makePointerSlice(groupNames),
		},
		{
			Name:   aws.String("vpc-id"),
			Values: []*string{&d.VpcId},
		},
	}

	groups, err := d.getClient().DescribeSecurityGroups(&ec2.DescribeSecurityGroupsInput{
		Filters: filters,
	})
	if err != nil {
		return err
	}

	var groupsByName = make(map[string]*ec2.SecurityGroup)
	for _, securityGroup := range groups.SecurityGroups {
		groupsByName[*securityGroup.GroupName] = securityGroup
	}

	for _, groupName := range groupNames {
		var group *ec2.SecurityGroup
		securityGroup, ok := groupsByName[groupName]
		if ok {
			log.Debugf("found existing security group (%s) in %s", groupName, d.VpcId)
			group = securityGroup
		} else {
			log.Debugf("creating security group (%s) in %s", groupName, d.VpcId)
			groupResp, err := d.getClient().CreateSecurityGroup(&ec2.CreateSecurityGroupInput{
				GroupName:   aws.String(groupName),
				Description: aws.String("Rancher Nodes"),
				VpcId:       aws.String(d.VpcId),
			})
			if err != nil && !strings.Contains(err.Error(), "already exists") {
				return err
			} else if err != nil {
				filters := []*ec2.Filter{
					{
						Name:   aws.String("group-name"),
						Values: []*string{aws.String(groupName)},
					},
					{
						Name:   aws.String("vpc-id"),
						Values: []*string{&d.VpcId},
					},
				}
				groups, err := d.getClient().DescribeSecurityGroups(&ec2.DescribeSecurityGroupsInput{
					Filters: filters,
				})
				if err != nil {
					return err
				}
				if len(groups.SecurityGroups) == 0 {
					return errors.New("can't find security group")
				}
				group = groups.SecurityGroups[0]
			}

			// Manually translate into the security group construct
			if group == nil {
				group = &ec2.SecurityGroup{
					GroupId:   groupResp.GroupId,
					VpcId:     aws.String(d.VpcId),
					GroupName: aws.String(groupName),
				}
			}

			_, err = d.getClient().CreateTags(&ec2.CreateTagsInput{
				Tags: []*ec2.Tag{
					{
						Key:   aws.String(machineTag),
						Value: aws.String(version),
					},
				},
				Resources: []*string{group.GroupId},
			})
			if err != nil && !strings.Contains(err.Error(), "already exists") {
				return fmt.Errorf("can't create tag for security group. err: %v", err)
			}

			// set Tag to group manually so that we know the group has rancher-nodes tag
			group.Tags = []*ec2.Tag{
				{
					Key:   aws.String(machineTag),
					Value: aws.String(version),
				},
			}

			// wait until created (dat eventual consistency)
			log.Debugf("waiting for group (%s) to become available", *group.GroupId)
			if err := mcnutils.WaitFor(d.securityGroupAvailableFunc(*group.GroupId)); err != nil {
				return err
			}
		}
		d.SecurityGroupIds = append(d.SecurityGroupIds, *group.GroupId)

		inboundPerms, err := d.configureSecurityGroupPermissions(group)
		if err != nil {
			return err
		}

		if len(inboundPerms) != 0 {
			log.Debugf("authorizing group %s with inbound permissions: %v", groupNames, inboundPerms)
			_, err := d.getClient().AuthorizeSecurityGroupIngress(&ec2.AuthorizeSecurityGroupIngressInput{
				GroupId:       group.GroupId,
				IpPermissions: inboundPerms,
			})
			if err != nil && !strings.Contains(err.Error(), "already exists") {
				return err
			}
		}

	}

	return nil
}

func (d *Driver) configureSecurityGroupPermissions(group *ec2.SecurityGroup) ([]*ec2.IpPermission, error) {
	hasPortsInbound := make(map[string]bool)
	for _, p := range group.IpPermissions {
		if p.FromPort != nil {
			hasPortsInbound[fmt.Sprintf("%d/%s", *p.FromPort, *p.IpProtocol)] = true
		}
	}

	inboundPerms := []*ec2.IpPermission{}

	if !hasPortsInbound["22/tcp"] {
		inboundPerms = append(inboundPerms, &ec2.IpPermission{
			IpProtocol: aws.String("tcp"),
			FromPort:   aws.Int64(22),
			ToPort:     aws.Int64(22),
			IpRanges:   []*ec2.IpRange{{CidrIp: aws.String(ipRange)}},
		})
	}

	if !hasPortsInbound[fmt.Sprintf("%d/tcp", dockerPort)] {
		inboundPerms = append(inboundPerms, &ec2.IpPermission{
			IpProtocol: aws.String("tcp"),
			FromPort:   aws.Int64(int64(dockerPort)),
			ToPort:     aws.Int64(int64(dockerPort)),
			IpRanges:   []*ec2.IpRange{{CidrIp: aws.String(ipRange)}},
		})
	}

	// we are only adding custom ports when the group is rancher-nodes
	if *group.GroupName == defaultSecurityGroup && hasTagKey(group.Tags, machineSecurityGroupName) {
		// kubeapi
		if !hasPortsInbound[fmt.Sprintf("%d/tcp", kubeApiPort)] {
			inboundPerms = append(inboundPerms, &ec2.IpPermission{
				IpProtocol: aws.String("tcp"),
				FromPort:   aws.Int64(int64(kubeApiPort)),
				ToPort:     aws.Int64(int64(kubeApiPort)),
				IpRanges:   []*ec2.IpRange{{CidrIp: aws.String(ipRange)}},
			})
		}

		// etcd
		if !hasPortsInbound[fmt.Sprintf("%d/tcp", etcdPorts[0])] {
			inboundPerms = append(inboundPerms, &ec2.IpPermission{
				IpProtocol: aws.String("tcp"),
				FromPort:   aws.Int64(int64(etcdPorts[0])),
				ToPort:     aws.Int64(int64(etcdPorts[1])),
				UserIdGroupPairs: []*ec2.UserIdGroupPair{
					{
						GroupId: group.GroupId,
					},
				},
			})
		}

		// vxlan
		if !hasPortsInbound[fmt.Sprintf("%d/udp", vxlanPorts[0])] {
			inboundPerms = append(inboundPerms, &ec2.IpPermission{
				IpProtocol: aws.String("udp"),
				FromPort:   aws.Int64(int64(vxlanPorts[0])),
				ToPort:     aws.Int64(int64(vxlanPorts[1])),
				UserIdGroupPairs: []*ec2.UserIdGroupPair{
					{
						GroupId: group.GroupId,
					},
				},
			})
		}

		// flannel
		if !hasPortsInbound[fmt.Sprintf("%d/udp", flannelPorts[0])] {
			inboundPerms = append(inboundPerms, &ec2.IpPermission{
				IpProtocol: aws.String("udp"),
				FromPort:   aws.Int64(int64(flannelPorts[0])),
				ToPort:     aws.Int64(int64(flannelPorts[1])),
				UserIdGroupPairs: []*ec2.UserIdGroupPair{
					{
						GroupId: group.GroupId,
					},
				},
			})
		}

		// others
		if !hasPortsInbound[fmt.Sprintf("%d/tcp", otherKubePorts[0])] {
			inboundPerms = append(inboundPerms, &ec2.IpPermission{
				IpProtocol: aws.String("tcp"),
				FromPort:   aws.Int64(int64(otherKubePorts[0])),
				ToPort:     aws.Int64(int64(otherKubePorts[1])),
				UserIdGroupPairs: []*ec2.UserIdGroupPair{
					{
						GroupId: group.GroupId,
					},
				},
			})
		}

		// kube proxy
		if !hasPortsInbound[fmt.Sprintf("%d/tcp", kubeProxyPorts[0])] {
			inboundPerms = append(inboundPerms, &ec2.IpPermission{
				IpProtocol: aws.String("tcp"),
				FromPort:   aws.Int64(int64(kubeProxyPorts[0])),
				ToPort:     aws.Int64(int64(kubeProxyPorts[1])),
				UserIdGroupPairs: []*ec2.UserIdGroupPair{
					{
						GroupId: group.GroupId,
					},
				},
			})
		}

		// node exporter
		if !hasPortsInbound[fmt.Sprintf("%d/tcp", nodeExporter)] {
			inboundPerms = append(inboundPerms, &ec2.IpPermission{
				IpProtocol: aws.String("tcp"),
				FromPort:   aws.Int64(int64(nodeExporter)),
				ToPort:     aws.Int64(int64(nodeExporter)),
				UserIdGroupPairs: []*ec2.UserIdGroupPair{
					{
						GroupId: group.GroupId,
					},
				},
			})
		}

		// nodePorts
		if !hasPortsInbound[fmt.Sprintf("%d/tcp", nodePorts[0])] {
			inboundPerms = append(inboundPerms, &ec2.IpPermission{
				IpProtocol: aws.String("tcp"),
				FromPort:   aws.Int64(int64(nodePorts[0])),
				ToPort:     aws.Int64(int64(nodePorts[1])),
				IpRanges:   []*ec2.IpRange{{CidrIp: aws.String(ipRange)}},
			})
		}

		if !hasPortsInbound[fmt.Sprintf("%d/udp", nodePorts[0])] {
			inboundPerms = append(inboundPerms, &ec2.IpPermission{
				IpProtocol: aws.String("udp"),
				FromPort:   aws.Int64(int64(nodePorts[0])),
				ToPort:     aws.Int64(int64(nodePorts[1])),
				IpRanges:   []*ec2.IpRange{{CidrIp: aws.String(ipRange)}},
			})
		}

		// nginx ingress
		if !hasPortsInbound[fmt.Sprintf("%d/tcp", httpPort)] {
			inboundPerms = append(inboundPerms, &ec2.IpPermission{
				IpProtocol: aws.String("tcp"),
				FromPort:   aws.Int64(int64(httpPort)),
				ToPort:     aws.Int64(int64(httpPort)),
				IpRanges:   []*ec2.IpRange{{CidrIp: aws.String(ipRange)}},
			})
		}

		if !hasPortsInbound[fmt.Sprintf("%d/tcp", httpsPort)] {
			inboundPerms = append(inboundPerms, &ec2.IpPermission{
				IpProtocol: aws.String("tcp"),
				FromPort:   aws.Int64(int64(httpsPort)),
				ToPort:     aws.Int64(int64(httpsPort)),
				IpRanges:   []*ec2.IpRange{{CidrIp: aws.String(ipRange)}},
			})
		}

		// calico additional port: https://docs.projectcalico.org/getting-started/openstack/requirements#network-requirements
		if !hasPortsInbound[fmt.Sprintf("%d/tcp", calicoPort)] {
			inboundPerms = append(inboundPerms, &ec2.IpPermission{
				IpProtocol: aws.String("tcp"),
				FromPort:   aws.Int64(int64(calicoPort)),
				ToPort:     aws.Int64(int64(calicoPort)),
				UserIdGroupPairs: []*ec2.UserIdGroupPair{
					{
						GroupId: group.GroupId,
					},
				},
			})
		}
	}

	for _, p := range d.OpenPorts {
		port, protocol := driverutil.SplitPortProto(p)
		portNum, err := strconv.ParseInt(port, 10, 0)
		if err != nil {
			return nil, fmt.Errorf("invalid port number %s: %s", port, err)
		}
		if !hasPortsInbound[fmt.Sprintf("%s/%s", port, protocol)] {
			inboundPerms = append(inboundPerms, &ec2.IpPermission{
				IpProtocol: aws.String(protocol),
				FromPort:   aws.Int64(portNum),
				ToPort:     aws.Int64(portNum),
				IpRanges:   []*ec2.IpRange{{CidrIp: aws.String(ipRange)}},
			})
		}
	}

	log.Debugf("configuring security group authorization for %s", ipRange)

	return inboundPerms, nil
}

func (d *Driver) deleteKeyPair() error {
	if d.KeyName == "" {
		log.Warn("Missing key pair name, this is likely due to a failure during machine creation")
		return nil
	}

	log.Debugf("deleting key pair: %s", d.KeyName)

	instance, err := d.getInstance()
	if err != nil {
		return err
	}

	_, err = d.getClient().DeleteKeyPair(&ec2.DeleteKeyPairInput{
		KeyName: instance.KeyName,
	})
	if err != nil {
		return err
	}

	return nil
}

func (d *Driver) getDefaultVPCId() (string, error) {
	output, err := d.getClient().DescribeAccountAttributes(&ec2.DescribeAccountAttributesInput{})
	if err != nil {
		return "", err
	}

	for _, attribute := range output.AccountAttributes {
		if *attribute.AttributeName == "default-vpc" {
			return *attribute.AttributeValues[0].AttributeValue, nil
		}
	}

	return "", errors.New("No default-vpc attribute")
}

func (d *Driver) getRegionZone() string {
	if d.Endpoint == "" {
		return d.Region + d.Zone
	}
	return d.Zone
}

func generateId() string {
	rb := make([]byte, 10)
	_, err := rand.Read(rb)
	if err != nil {
		log.Warnf("Unable to generate id: %s", err)
	}

	h := md5.New()
	io.WriteString(h, string(rb))
	return fmt.Sprintf("%x", h.Sum(nil))
}

func hasTagKey(tags []*ec2.Tag, key string) bool {
	for _, tag := range tags {
		if *tag.Key == key {
			return true
		}
	}
	return false
}

func (d *Driver) updateBDMList() []*ec2.BlockDeviceMapping {
	var bdmList []*ec2.BlockDeviceMapping

	for _, bdm := range d.bdmList {
		if bdm.Ebs != nil {
			if *bdm.DeviceName == d.DeviceName {
				bdm.Ebs.VolumeSize = aws.Int64(d.RootSize)
				bdm.Ebs.VolumeType = aws.String(d.VolumeType)
			}
			bdm.Ebs.DeleteOnTermination = aws.Bool(true)
			bdmList = append(bdmList, bdm)
		}
	}

	return bdmList
}
