package validations

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strings"

	"github.com/asaskevich/govalidator"
	"github.com/containers/image/v5/docker/reference"
	"github.com/openshift/assisted-service/internal/common"
	"github.com/openshift/assisted-service/internal/network"
	"github.com/openshift/assisted-service/pkg/auth"
	"github.com/openshift/assisted-service/pkg/ocm"
	"github.com/pkg/errors"
	"golang.org/x/crypto/ssh"
)

type Config struct {
	PublicRegistries string `envconfig:"PUBLIC_CONTAINER_REGISTRIES" default:""`
}

const (
	clusterNameRegex    = "^([a-z]([-a-z0-9]*[a-z0-9])?)*$"
	dnsNameRegex        = "^([a-z0-9]+(-[a-z0-9]+)*[.])+[a-z]{2,}$"
	hostnameRegex       = `^(([a-zA-Z0-9]|[a-zA-Z0-9][a-zA-Z0-9\-]*[a-zA-Z0-9])\.)*([A-Za-z0-9]|[A-Za-z0-9][A-Za-z0-9\-]*[A-Za-z0-9])$`
	CloudOpenShiftCom   = "cloud.openshift.com"
	sshPublicKeyRegex   = "^(ssh-rsa AAAAB3NzaC1yc2|ecdsa-sha2-nistp256 AAAAE2VjZHNhLXNoYTItbmlzdHAyNT|ecdsa-sha2-nistp384 AAAAE2VjZHNhLXNoYTItbmlzdHAzODQAAAAIbmlzdHAzOD|ecdsa-sha2-nistp521 AAAAE2VjZHNhLXNoYTItbmlzdHA1MjEAAAAIbmlzdHA1Mj|ssh-ed25519 AAAAC3NzaC1lZDI1NTE5|ssh-dss AAAAB3NzaC1kc3)[0-9A-Za-z+/]+[=]{0,3}( .*)?$"
	dockerHubRegistry   = "docker.io"
	dockerHubLegacyAuth = "https://index.docker.io/v1/"
	stageRegistry       = "registry.stage.redhat.io"
	ignoreListSeparator = ","
)

var regexpSshPublicKey *regexp.Regexp

func init() {
	regexpSshPublicKey, _ = regexp.Compile(sshPublicKeyRegex)
}

// PullSecretValidator is used run validations on a provided pull secret
// it verifies the format of the pull secrete and access to required image registries
//go:generate mockgen -source=validations.go -package=validations -destination=mock_validations.go
type PullSecretValidator interface {
	ValidatePullSecret(secret string, username string, authHandler auth.Authenticator) error
}

type registryPullSecretValidator struct {
	registriesWithAuth *map[string]bool
}

type imagePullSecret struct {
	Auths map[string]map[string]interface{} `json:"auths"`
}

type PullSecretCreds struct {
	Username string
	Password string
	Registry string
	AuthRaw  string
}

// PullSecretError distinguishes secret validation errors produced by this package from other types of errors
type PullSecretError struct {
	Msg   string
	Cause error
}

func (e *PullSecretError) Error() string {
	return e.Msg
}

func (e *PullSecretError) Unwrap() error {
	return e.Cause
}

// ParsePullSecret validates the format of a pull secret and converts the secret string into individual credentail entries
func ParsePullSecret(secret string) (map[string]PullSecretCreds, error) {
	result := make(map[string]PullSecretCreds)
	var s imagePullSecret

	err := json.Unmarshal([]byte(strings.TrimSpace(secret)), &s)
	if err != nil {
		return nil, &PullSecretError{Msg: "pull secret must be a well-formed JSON", Cause: err}
	}

	if len(s.Auths) == 0 {
		return nil, &PullSecretError{Msg: "pull secret must contain 'auths' JSON-object field"}
	}

	for d, a := range s.Auths {

		_, authPresent := a["auth"]
		_, credsStorePresent := a["credsStore"]
		if !authPresent && !credsStorePresent {
			return nil, &PullSecretError{Msg: fmt.Sprintf("invalid pull secret: %q JSON-object requires either 'auth' or 'credsStore' field", d)}
		}

		data, err := base64.StdEncoding.DecodeString(a["auth"].(string))
		if err != nil {
			return nil, &PullSecretError{Msg: fmt.Sprintf("invalid pull secret: 'auth' fields of %q are not base64-encoded", d)}
		}

		res := bytes.Split(data, []byte(":"))
		if len(res) != 2 {
			return nil, &PullSecretError{Msg: fmt.Sprintf("invalid pull secret: 'auth' for %s is not in 'user:password' format", d)}
		}
		result[d] = PullSecretCreds{
			Password: string(res[1]),
			Username: string(res[0]),
			AuthRaw:  a["auth"].(string),
			Registry: d,
		}

	}
	return result, nil
}

func AddRHRegPullSecret(secret, rhCred string) (string, error) {
	if rhCred == "" {
		return "", errors.Errorf("invalid pull secret")
	}
	var s imagePullSecret
	err := json.Unmarshal([]byte(strings.TrimSpace(secret)), &s)
	if err != nil {
		return secret, errors.Errorf("invalid pull secret: %v", err)
	}
	s.Auths[stageRegistry] = make(map[string]interface{})
	s.Auths[stageRegistry]["auth"] = base64.StdEncoding.EncodeToString([]byte(rhCred))
	ps, err := json.Marshal(s)
	if err != nil {
		return secret, err
	}
	return string(ps), nil
}

// NewPullSecretValidator receives all images whose registries must have an entry in a user pull secret (auth)
func NewPullSecretValidator(config Config, images ...string) (PullSecretValidator, error) {

	authRegList, err := getRegistriesWithAuth(config.PublicRegistries, ignoreListSeparator, images...)
	if err != nil {
		return nil, err
	}

	return &registryPullSecretValidator{
		registriesWithAuth: authRegList,
	}, nil
}

// ValidatePullSecret validates that a pull secret is well formed and contains all required data
func (v *registryPullSecretValidator) ValidatePullSecret(secret string, username string, authHandler auth.Authenticator) error {
	creds, err := ParsePullSecret(secret)
	if err != nil {
		return err
	}

	// only check for cloud creds if we're authenticating against Red Hat SSO
	if authHandler.AuthType() == auth.TypeRHSSO {

		r, ok := creds["cloud.openshift.com"]
		if !ok {
			return &PullSecretError{Msg: "pull secret must contain auth for \"cloud.openshift.com\""}
		}

		user, err := authHandler.AuthAgentAuth(r.AuthRaw)
		if err != nil {
			return &PullSecretError{Msg: "failed to authenticate the pull secret token"}
		}

		if (user.(*ocm.AuthPayload)).Username != username {
			return &PullSecretError{Msg: "pull secret token does not match current user"}
		}
	}

	for registry := range *v.registriesWithAuth {

		// Both "docker.io" and "https://index.docker.io/v1/" are acceptable for DockerHub login
		if registry == dockerHubRegistry {
			if _, ok := creds[dockerHubLegacyAuth]; ok {
				continue
			}
		}

		// We add auth for stage registry automatically
		if registry == stageRegistry {
			continue
		}

		if _, ok := creds[registry]; !ok {
			return &PullSecretError{Msg: fmt.Sprintf("pull secret must contain auth for %q", registry)}
		}
	}

	return nil
}

func ValidateDomainNameFormat(dnsDomainName string) error {
	matched, err := regexp.MatchString(dnsNameRegex, dnsDomainName)
	if err != nil {
		return common.NewApiError(http.StatusInternalServerError, errors.Wrapf(err, "DNS name validation for %s", dnsDomainName))
	}
	if !matched {
		return common.NewApiError(http.StatusBadRequest, errors.Errorf("DNS format mismatch: %s domain name is not valid", dnsDomainName))
	}
	return nil
}

func ValidateHostname(name string) error {
	matched, err := regexp.MatchString(hostnameRegex, name)
	if err != nil {
		return errors.Wrapf(err, "Hostname validation for %s", name)
	}
	if !matched {
		return errors.Errorf("Hostname format mismatch: %s name is not valid", name)
	}
	return nil
}

func ValidateAdditionalNTPSource(commaSeparatedNTPSources string) bool {
	return common.AllStrings(strings.Split(commaSeparatedNTPSources, ","), ValidateNTPSource)
}

func ValidateNTPSource(ntpSource string) bool {
	if addr := net.ParseIP(ntpSource); addr != nil {
		return true
	}

	if err := ValidateHostname(ntpSource); err == nil {
		return true
	}

	return false
}

// ValidateClusterNameFormat validates specified cluster name format
func ValidateClusterNameFormat(name string) error {
	if matched, _ := regexp.MatchString(clusterNameRegex, name); !matched {
		return errors.Errorf("Cluster name format is not valid: '%s'. "+
			"Name must consist of lower-case letters, numbers and hyphens. "+
			"It must start with a letter and end with a letter or number.", name)
	}
	return nil
}

// ValidateHTTPProxyFormat validates the HTTP Proxy and HTTPS Proxy format
func ValidateHTTPProxyFormat(proxyURL string) error {
	if !govalidator.IsURL(proxyURL) {
		return errors.Errorf("Proxy URL format is not valid: '%s'", proxyURL)
	}
	u, err := url.Parse(proxyURL)
	if err != nil {
		return errors.Errorf("Proxy URL format is not valid: '%s'", proxyURL)
	}
	if u.Scheme == "https" {
		return errors.Errorf("The URL scheme must be http; https is currently not supported: '%s'", proxyURL)
	}
	if u.Scheme != "http" {
		return errors.Errorf("The URL scheme must be http and specified in the URL: '%s'", proxyURL)
	}
	return nil
}

// ValidateNoProxyFormat validates the no-proxy format which should be a comma-separated list
// of destination domain names, domains, IP addresses or other network CIDRs. A domain can be
// prefaced with '.' to include all subdomains of that domain.
// Use '*' to bypass proxy for all destinations in OCP 4.8 or later.
func ValidateNoProxyFormat(noProxy string, ocpVersion string) error {
	if noProxy == "*" {
		if wildcardSupported, err := common.VersionGreaterOrEqual(ocpVersion, "4.8.0-fc.4"); err != nil {
			return err
		} else if wildcardSupported {
			return nil
		}
		return errors.Errorf("Sorry, no-proxy value '*' is not supported in this release")
	}
	domains := strings.Split(noProxy, ",")
	for _, s := range domains {
		s = strings.TrimPrefix(s, ".")
		if govalidator.IsIP(s) {
			continue
		}

		if govalidator.IsCIDR(s) {
			continue
		}

		if govalidator.IsDNSName(s) {
			continue
		}
		return errors.Errorf("NO Proxy format is not valid: '%s'. "+
			"NO Proxy is a comma-separated list of destination domain names, domains, IP addresses or other network CIDRs. "+
			"A domain can be prefaced with '.' to include all subdomains of that domain. Use '*' to bypass proxy for all destinations with OpenShift 4.8 or later.", noProxy)
	}
	return nil
}

func ValidateSSHPublicKey(sshPublicKeys string) error {
	if regexpSshPublicKey == nil {
		return fmt.Errorf("Can't parse SSH keys.")
	}

	for _, sshPublicKey := range strings.Split(sshPublicKeys, "\n") {
		sshPublicKey = strings.TrimSpace(sshPublicKey)
		keyBytes := []byte(sshPublicKey)
		isMatched := regexpSshPublicKey.Match(keyBytes)
		if !isMatched {
			return errors.Errorf(
				"SSH key: %s does not match any supported type: ssh-rsa, ssh-ed25519, ecdsa-[VARIANT]",
				sshPublicKey)
		} else if _, _, _, _, err := ssh.ParseAuthorizedKey(keyBytes); err != nil {
			return errors.Wrapf(err, fmt.Sprintf("Malformed SSH key: %s", sshPublicKey))
		}
	}

	return nil
}

// ParseRegistry extracts the registry from a full image name, or returns
// the default if the name does not start with a registry.
func ParseRegistry(image string) (string, error) {
	parsed, err := reference.ParseNormalizedNamed(strings.TrimSpace(image))
	if err != nil {
		return "", err
	}
	return reference.Domain(parsed), nil
}

// getRegistriesWithAuth returns container registries that may require authentication based
// on a list of used images and an ignore list. The ingore list comes as a string and a separator
// to make it easier to read from a configuration variable
func getRegistriesWithAuth(ignoreList string, ignoreSeparator string, images ...string) (*map[string]bool, error) {

	ignored := make(map[string]bool)
	for _, i := range strings.Split(ignoreList, ignoreSeparator) {
		ignored[i] = true
	}

	_, docLegacyIgnored := ignored[dockerHubLegacyAuth]

	registries := make(map[string]bool)
	for _, img := range images {
		if img == "" {
			continue
		}
		r, err := ParseRegistry(img)
		if err != nil {
			return &registries, err
		}

		if r == dockerHubRegistry && docLegacyIgnored {
			continue
		}

		if _, ok := ignored[r]; ok {
			continue
		}

		registries[r] = true
	}

	return &registries, nil
}

//ValidateVipDHCPAllocationWithIPv6 returns an error in case of VIP DHCP allocation
//being used with IPv6 machine network
func ValidateVipDHCPAllocationWithIPv6(vipDhcpAllocation bool, machineNetworkCIDR string) error {
	if !vipDhcpAllocation {
		return nil
	}
	if network.IsIPv6CIDR(machineNetworkCIDR) {
		return errors.Errorf("VIP DHCP allocation is unsupported with IPv6 network")
	}
	return nil
}

//ValidateIPAddressFamily returns an error if the argument contains an IP address
// or CIDR of IPv6 family, and IPv6 support is turned off
func ValidateIPAddressFamily(ipV6Supported bool, elements ...*string) error {
	if ipV6Supported {
		return nil
	}
	for _, e := range elements {
		if e == nil {
			continue
		}
		if strings.Contains(*e, ":") {
			return errors.Errorf("IPv6 is not supported in this setup")
		}
	}
	return nil
}
