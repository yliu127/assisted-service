package common

import (
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net"

	"github.com/go-openapi/strfmt"
	. "github.com/onsi/gomega"
	"github.com/openshift/assisted-service/internal/constants"
	"github.com/openshift/assisted-service/models"
	"github.com/openshift/assisted-service/pkg/conversions"
	"github.com/sirupsen/logrus"
	"gopkg.in/yaml.v2"
)

type TestNetworking struct {
	ClusterNetworkCidr       string
	ClusterNetworkHostPrefix int64
	ServiceNetworkCidr       string
	MachineNetworkCidr       string
	APIVip                   string
	IngressVip               string
}

type TestConfiguration struct {
	OpenShiftVersion string
	ReleaseVersion   string
	ReleaseImage     string
	RhcosImage       string
	RhcosVersion     string
	SupportLevel     string
	Version          *models.OpenshiftVersion

	Status            string
	StatusInfo        string
	HostProgressStage models.HostStage

	Disks         *models.Disk
	ImageName     string
	ClusterName   string
	BaseDNSDomain string

	MonitoredOperator models.MonitoredOperator
}

const TestDiskId = "/dev/disk/by-id/test-disk-id"
const TestDiskPath = "/dev/test-disk"

var (
	OpenShiftVersion string = "4.6"
	ReleaseVersion          = "4.6.0"
	ReleaseImage            = "quay.io/openshift-release-dev/ocp-release:4.6.16-x86_64"
	RhcosImage              = "rhcos_4.6.0"
	RhcosVersion            = "version-46.123-0"
	SupportLevel            = "beta"
)

// Defaults to be used by all testing modules
var TestDefaultConfig = &TestConfiguration{
	OpenShiftVersion: OpenShiftVersion,
	ReleaseVersion:   ReleaseVersion,
	ReleaseImage:     ReleaseImage,
	Version: &models.OpenshiftVersion{
		DisplayName:    &OpenShiftVersion,
		ReleaseImage:   &ReleaseImage,
		ReleaseVersion: &ReleaseVersion,
		RhcosImage:     &RhcosImage,
		RhcosVersion:   &RhcosVersion,
		SupportLevel:   &SupportLevel,
	},
	Status:            "status",
	StatusInfo:        "statusInfo",
	HostProgressStage: models.HostStage("default progress stage"),

	Disks: &models.Disk{
		ID:     TestDiskId,
		Name:   "test-disk",
		Serial: "test-serial",
		InstallationEligibility: models.DiskInstallationEligibility{
			Eligible:           false,
			NotEligibleReasons: []string{"Bad disk"},
		},
	},

	ImageName: "image",

	ClusterName: "test",

	BaseDNSDomain: "example.com",

	MonitoredOperator: models.MonitoredOperator{
		Name:         "dummy",
		OperatorType: models.OperatorTypeBuiltin,
	},
}

var TestNTPSourceSynced = &models.NtpSource{SourceName: "clock.dummy.test", SourceState: models.SourceStateSynced}
var TestNTPSourceUnsynced = &models.NtpSource{SourceName: "2.2.2.2", SourceState: models.SourceStateUnreachable}
var TestImageStatusesSuccess = &models.ContainerImageAvailability{
	Name:         TestDefaultConfig.ImageName,
	Result:       models.ContainerImageAvailabilityResultSuccess,
	SizeBytes:    333000000.0,
	Time:         10.0,
	DownloadRate: 33.3,
}
var TestImageStatusesFailure = &models.ContainerImageAvailability{
	Name:   TestDefaultConfig.ImageName,
	Result: models.ContainerImageAvailabilityResultFailure,
}

var DomainAPI = "api.test.example.com"
var DomainAPIInternal = "api-int.test.example.com"
var DomainApps = fmt.Sprintf("%s.apps.test.example.com", constants.AppsSubDomainNameHostDNSValidation)

var DomainResolution = []*models.DomainResolutionResponseDomain{
	{
		DomainName:    &DomainAPI,
		IPV4Addresses: []strfmt.IPv4{"1.2.3.4/24"},
		IPV6Addresses: []strfmt.IPv6{"1001:db8::10/120"},
	},
	{
		DomainName:    &DomainAPIInternal,
		IPV4Addresses: []strfmt.IPv4{"4.5.6.7/24"},
		IPV6Addresses: []strfmt.IPv6{"1002:db8::10/120"},
	},
	{
		DomainName:    &DomainApps,
		IPV4Addresses: []strfmt.IPv4{"7.8.9.10/24"},
		IPV6Addresses: []strfmt.IPv6{"1003:db8::10/120"},
	}}

var TestDomainNameResolutionSuccess = &models.DomainResolutionResponse{
	Resolutions: DomainResolution}

var TestDefaultRouteConfiguration = []*models.Route{{Family: FamilyIPv4, Interface: "eth0", Gateway: "192.168.1.1", Destination: "0.0.0.0"}}

var TestIPv4Networking = TestNetworking{
	ClusterNetworkCidr:       "1.3.0.0/16",
	ClusterNetworkHostPrefix: 24,
	ServiceNetworkCidr:       "1.2.5.0/24",
	MachineNetworkCidr:       "1.2.3.0/24",
	APIVip:                   "1.2.3.5",
	IngressVip:               "1.2.3.6",
}

var TestIPv6Networking = TestNetworking{
	ClusterNetworkCidr:       "1003:db8::/53",
	ClusterNetworkHostPrefix: 64,
	ServiceNetworkCidr:       "1002:db8::/119",
	MachineNetworkCidr:       "1001:db8::/120",
	APIVip:                   "1001:db8::64",
	IngressVip:               "1001:db8::65",
}

func IncrementCidrIP(subnet string) string {
	_, cidr, _ := net.ParseCIDR(subnet)
	IncrementIP(cidr.IP)
	return cidr.String()
}

func IncrementIPString(ipString string) string {
	ip := net.ParseIP(ipString)
	IncrementIP(ip)
	return ip.String()
}

func IncrementIP(ip net.IP) {
	for j := len(ip) - 1; j >= 0; j-- {
		ip[j]++
		if ip[j] > 0 {
			break
		}
	}
}

func IncrementCidrMask(subnet string) string {
	_, cidr, _ := net.ParseCIDR(subnet)
	ones, bits := cidr.Mask.Size()
	cidr.Mask = net.CIDRMask(ones+1, bits)
	return cidr.String()
}

func GenerateTestDefaultInventory() string {
	inventory := &models.Inventory{
		Interfaces: []*models.Interface{
			{
				Name: "eth0",
				IPV4Addresses: []string{
					"1.2.3.4/24",
				},
				IPV6Addresses: []string{
					"1001:db8::10/120",
				},
			},
		},
		Disks: []*models.Disk{
			TestDefaultConfig.Disks,
		},
		Routes: TestDefaultRouteConfiguration,
	}

	b, err := json.Marshal(inventory)
	Expect(err).To(Not(HaveOccurred()))
	return string(b)
}

func GenerateTestDefaultVmwareInventory() string {
	inventory := &models.Inventory{
		Interfaces: []*models.Interface{
			{
				Name: "eth0",
				IPV4Addresses: []string{
					"1.2.3.4/24",
				},
				IPV6Addresses: []string{
					"1001:db8::10/120",
				},
			},
		},
		Disks: []*models.Disk{
			TestDefaultConfig.Disks,
		},
		SystemVendor: &models.SystemVendor{
			Manufacturer: "vmware",
		},
		Routes: TestDefaultRouteConfiguration,
	}

	b, err := json.Marshal(inventory)
	Expect(err).To(Not(HaveOccurred()))
	return string(b)
}

type NetAddress struct {
	IPv4Address []string
	IPv6Address []string
	Hostname    string
}

func GenerateTestInventoryWithNetwork(netAddress NetAddress) string {
	inventory := &models.Inventory{
		Interfaces: []*models.Interface{
			{
				Name:          "eth0",
				IPV4Addresses: netAddress.IPv4Address,
				IPV6Addresses: netAddress.IPv6Address,
			},
		},
		Disks:        []*models.Disk{{SizeBytes: conversions.GibToBytes(120), DriveType: "HDD"}},
		CPU:          &models.CPU{Count: 16},
		Memory:       &models.Memory{PhysicalBytes: conversions.GibToBytes(16), UsableBytes: conversions.GibToBytes(16)},
		SystemVendor: &models.SystemVendor{Manufacturer: "Red Hat", ProductName: "RHEL", SerialNumber: "3534"},
		Hostname:     netAddress.Hostname,
		Routes:       TestDefaultRouteConfiguration,
	}
	b, err := json.Marshal(inventory)
	Expect(err).To(Not(HaveOccurred()))
	return string(b)
}

func GenerateTestInventoryWithSetNetwork() string {
	inventory := &models.Inventory{
		Interfaces: []*models.Interface{
			{
				Name: "eth0",
				IPV4Addresses: []string{
					"1.2.3.4/24",
				},
				IPV6Addresses: []string{
					"1001:db8::10/120",
				},
			},
		},
		Disks:        []*models.Disk{{SizeBytes: conversions.GibToBytes(120), DriveType: "HDD"}},
		CPU:          &models.CPU{Count: 16},
		Memory:       &models.Memory{PhysicalBytes: conversions.GibToBytes(16), UsableBytes: conversions.GibToBytes(16)},
		SystemVendor: &models.SystemVendor{Manufacturer: "Red Hat", ProductName: "RHEL", SerialNumber: "3534"},
		Routes:       TestDefaultRouteConfiguration,
	}
	b, err := json.Marshal(inventory)
	Expect(err).To(Not(HaveOccurred()))
	return string(b)
}

func GetTestLog() logrus.FieldLogger {
	l := logrus.New()
	l.SetOutput(ioutil.Discard)
	return l
}

type StaticNetworkConfig struct {
	DNSResolver DNSResolver  `yaml:"dns-resolver"`
	Interfaces  []Interfaces `yaml:"interfaces"`
	Routes      Routes       `yaml:"routes"`
}
type DNSResolverConfig struct {
	Server []string `yaml:"server"`
}
type DNSResolver struct {
	Config DNSResolverConfig `yaml:"config"`
}
type Address struct {
	IP           string `yaml:"ip"`
	PrefixLength int    `yaml:"prefix-length"`
}
type Ipv4 struct {
	Address []Address `yaml:"address"`
	Dhcp    bool      `yaml:"dhcp"`
	Enabled bool      `yaml:"enabled"`
}
type Interfaces struct {
	Ipv4  Ipv4   `yaml:"ipv4"`
	Name  string `yaml:"name"`
	State string `yaml:"state"`
	Type  string `yaml:"type"`
}
type RouteConfig struct {
	Destination      string `yaml:"destination"`
	NextHopAddress   string `yaml:"next-hop-address"`
	NextHopInterface string `yaml:"next-hop-interface"`
	TableID          int    `yaml:"table-id"`
}
type Routes struct {
	Config []RouteConfig `yaml:"config"`
}

func FormatStaticConfigHostYAML(nicPrimary, nicSecondary, ip4Master, ip4Secondary, dnsGW string, macInterfaceMap models.MacInterfaceMap) *models.HostStaticNetworkConfig {
	staticNetworkConfig := StaticNetworkConfig{
		DNSResolver: DNSResolver{
			Config: DNSResolverConfig{
				Server: []string{dnsGW},
			},
		},
		Interfaces: []Interfaces{
			{
				Ipv4: Ipv4{
					Address: []Address{
						{
							IP:           ip4Master,
							PrefixLength: 24,
						},
					},
					Dhcp:    false,
					Enabled: true,
				},
				Name:  nicPrimary,
				State: "up",
				Type:  "ethernet",
			},
			{
				Ipv4: Ipv4{
					Address: []Address{
						{
							IP:           ip4Secondary,
							PrefixLength: 24,
						},
					},
					Dhcp:    false,
					Enabled: true,
				},
				Name:  nicSecondary,
				State: "up",
				Type:  "ethernet",
			},
		},
		Routes: Routes{
			Config: []RouteConfig{
				{
					Destination:      "0.0.0.0/0",
					NextHopAddress:   dnsGW,
					NextHopInterface: nicPrimary,
					TableID:          254,
				},
			},
		},
	}

	output, _ := yaml.Marshal(staticNetworkConfig)
	return &models.HostStaticNetworkConfig{MacInterfaceMap: macInterfaceMap, NetworkYaml: string(output)}
}
