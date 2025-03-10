package subsystem

import (
	"context"
	"fmt"
	"net/url"
	"testing"
	"time"

	"github.com/go-openapi/runtime"
	"github.com/jinzhu/gorm"
	_ "github.com/jinzhu/gorm/dialects/postgres"
	"github.com/kelseyhightower/envconfig"
	bmh_v1alpha1 "github.com/metal3-io/baremetal-operator/apis/metal3.io/v1alpha1"
	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
	hiveext "github.com/openshift/assisted-service/api/hiveextension/v1beta1"
	"github.com/openshift/assisted-service/api/v1beta1"
	"github.com/openshift/assisted-service/client"
	"github.com/openshift/assisted-service/client/versions"
	"github.com/openshift/assisted-service/pkg/auth"
	hivev1 "github.com/openshift/hive/apis/hive/v1"
	"github.com/sirupsen/logrus"
	"k8s.io/client-go/kubernetes/scheme"
	k8sclient "sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/config"
)

var db *gorm.DB
var agentBMClient, badAgentBMClient, userBMClient, readOnlyAdminUserBMClient, unallowedUserBMClient *client.AssistedInstall
var log *logrus.Logger
var wiremock *WireMock
var kubeClient k8sclient.Client
var openshiftVersion string = "4.6"
var snoVersion string = "4.8"

const (
	pollDefaultInterval = 1 * time.Millisecond
	pollDefaultTimeout  = 30 * time.Second
)

var Options struct {
	DBHost                  string        `envconfig:"DB_HOST"`
	DBPort                  string        `envconfig:"DB_PORT"`
	AuthType                auth.AuthType `envconfig:"AUTH_TYPE"`
	InventoryHost           string        `envconfig:"INVENTORY"`
	TestToken               string        `envconfig:"TEST_TOKEN"`
	TestTokenAdmin          string        `envconfig:"TEST_TOKEN_ADMIN"`
	TestTokenUnallowed      string        `envconfig:"TEST_TOKEN_UNALLOWED"`
	OCMHost                 string        `envconfig:"OCM_HOST"`
	DeployTarget            string        `envconfig:"DEPLOY_TARGET" default:"k8s"`
	Storage                 string        `envconfig:"STORAGE" default:""`
	Namespace               string        `envconfig:"NAMESPACE" default:"assisted-installer"`
	EnableKubeAPI           bool          `envconfig:"ENABLE_KUBE_API" default:"false"`
	DeregisterInactiveAfter time.Duration `envconfig:"DELETED_INACTIVE_AFTER" default:"480h"` // 20d
}

func clientcfg(authInfo runtime.ClientAuthInfoWriter) client.Config {
	cfg := client.Config{
		URL: &url.URL{
			Scheme: client.DefaultSchemes[0],
			Host:   Options.InventoryHost,
			Path:   client.DefaultBasePath,
		},
	}
	if Options.AuthType != auth.TypeNone {
		log.Info("API Key authentication enabled for subsystem tests")
		cfg.AuthInfo = authInfo
	}
	return cfg
}

func setupKubeClient() {
	if addErr := v1beta1.AddToScheme(scheme.Scheme); addErr != nil {
		logrus.Fatalf("Fail adding kubernetes v1beta1 scheme: %s", addErr)
	}
	if addErr := hivev1.AddToScheme(scheme.Scheme); addErr != nil {
		logrus.Fatalf("Fail adding kubernetes hivev1 scheme: %s", addErr)
	}
	if addErr := hiveext.AddToScheme(scheme.Scheme); addErr != nil {
		logrus.Fatalf("Fail adding kubernetes hivev1 scheme: %s", addErr)
	}
	if addErr := bmh_v1alpha1.AddToScheme(scheme.Scheme); addErr != nil {
		logrus.Fatalf("Fail adding kubernetes bmh scheme: %s", addErr)
	}

	var err error
	kubeClient, err = k8sclient.New(config.GetConfigOrDie(), k8sclient.Options{Scheme: scheme.Scheme})
	if err != nil {
		logrus.Fatalf("Fail adding kubernetes client: %s", err)
	}
}

func init() {
	var err error
	log = logrus.New()
	log.SetReportCaller(true)
	err = envconfig.Process("subsystem", &Options)
	if err != nil {
		log.Fatal(err.Error())
	}
	userClientCfg := clientcfg(auth.UserAuthHeaderWriter("bearer " + Options.TestToken))
	adminUserClientCfg := clientcfg(auth.UserAuthHeaderWriter("bearer " + Options.TestTokenAdmin))
	unallowedUserClientCfg := clientcfg(auth.UserAuthHeaderWriter("bearer " + Options.TestTokenUnallowed))
	agentClientCfg := clientcfg(auth.AgentAuthHeaderWriter(FakePS))
	badAgentClientCfg := clientcfg(auth.AgentAuthHeaderWriter(WrongPullSecret))
	userBMClient = client.New(userClientCfg)
	readOnlyAdminUserBMClient = client.New(adminUserClientCfg)
	unallowedUserBMClient = client.New(unallowedUserClientCfg)
	agentBMClient = client.New(agentClientCfg)
	badAgentBMClient = client.New(badAgentClientCfg)

	db, err = gorm.Open("postgres",
		fmt.Sprintf("host=%s port=%s user=admin dbname=installer password=admin sslmode=disable",
			Options.DBHost, Options.DBPort))
	if err != nil {
		logrus.Fatal("Fail to connect to DB, ", err)
	}

	if Options.EnableKubeAPI {
		setupKubeClient()
	}

	if Options.AuthType == auth.TypeRHSSO {
		wiremock = &WireMock{
			OCMHost:   Options.OCMHost,
			TestToken: Options.TestToken,
		}
		err = wiremock.DeleteAllWiremockStubs()
		if err != nil {
			logrus.Fatal("Fail to delete all wiremock stubs, ", err)
		}

		if err = wiremock.CreateWiremockStubsForOCM(); err != nil {
			logrus.Fatal("Failed to init wiremock stubs, ", err)
		}
	}

	// Test on first openshift version. We can't test on latest because quay.io/openshift-release-dev/ocp-release-nightly
	// is not public, so we would have to add PULL_SECRET env var as mandatory to access the quay.
	if reply, err := userBMClient.Versions.ListSupportedOpenshiftVersions(context.Background(),
		&versions.ListSupportedOpenshiftVersionsParams{}); err == nil {
		for openshiftVersion = range reply.GetPayload() {
			break
		}
	}
}

func TestSubsystem(t *testing.T) {
	RegisterFailHandler(Fail)
	clearDB()
	RunSpecs(t, "Subsystem Suite")
}
