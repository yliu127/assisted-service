package hostcommands

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/jinzhu/gorm"
	"github.com/openshift/assisted-service/internal/common"
	"github.com/openshift/assisted-service/internal/constants"
	"github.com/openshift/assisted-service/models"
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
)

type domainNameResolutionCmd struct {
	baseCmd
	domainNameResolutionImage string
	db                        *gorm.DB
}

func NewDomainNameResolutionCmd(log logrus.FieldLogger, domainNameResolutionImage string, db *gorm.DB) *domainNameResolutionCmd {
	return &domainNameResolutionCmd{
		baseCmd:                   baseCmd{log: log},
		domainNameResolutionImage: domainNameResolutionImage,
		db:                        db,
	}
}

func (f *domainNameResolutionCmd) prepareParam(host *models.Host, cluster *common.Cluster) (string, error) {
	clusterName := cluster.Cluster.Name
	if len(clusterName) == 0 {
		err := errors.Errorf("Cluster name is empty for cluster %s", host.ClusterID)
		f.log.WithError(err).Warn("Cluster name is empty")
		return "", err
	}

	baseDNSDomain := cluster.Cluster.BaseDNSDomain
	if len(baseDNSDomain) == 0 {
		err := errors.Errorf("Cluster base domain is empty for cluster %s", host.ClusterID)
		f.log.WithError(err).Warn("Cluster base domain is empty")
		return "", err
	}
	apiDomainName := fmt.Sprintf("api.%s.%s", clusterName, baseDNSDomain)
	apiInternalDomainName := fmt.Sprintf("api-int.%s.%s", clusterName, baseDNSDomain)
	appsDomainName := fmt.Sprintf("%s.apps.%s.%s", constants.AppsSubDomainNameHostDNSValidation, clusterName, baseDNSDomain)

	apiDomain := models.DomainResolutionRequestDomain{
		DomainName: &apiDomainName,
	}
	apiInternalDomain := models.DomainResolutionRequestDomain{
		DomainName: &apiInternalDomainName,
	}
	appsDomain := models.DomainResolutionRequestDomain{
		DomainName: &appsDomainName,
	}

	var domains []*models.DomainResolutionRequestDomain
	domains = append(domains, &apiDomain, &apiInternalDomain, &appsDomain)

	request := models.DomainResolutionRequest{
		Domains: domains,
	}

	b, err := json.Marshal(&request)
	if err != nil {
		f.log.WithError(err).Warn("Json marshal")
		return "", err
	}
	return string(b), nil
}

func (f *domainNameResolutionCmd) GetSteps(ctx context.Context, host *models.Host) ([]*models.Step, error) {
	var cluster common.Cluster
	if err := f.db.First(&cluster, "id = ?", host.ClusterID).Error; err != nil {
		f.log.WithError(err).Errorf("failed to fetch cluster %s", host.ClusterID)
		return nil, err
	}
	param, err := f.prepareParam(host, &cluster)
	if err != nil {
		return nil, err
	}

	step := &models.Step{
		StepType: models.StepTypeDomainResolution,
		Command:  "podman",
		Args: []string{
			"run", "--privileged", "--net=host", "--rm", "--quiet",
			"-v", "/var/log:/var/log",
			"-v", "/run/systemd/journal/socket:/run/systemd/journal/socket",
			f.domainNameResolutionImage,
			"domain_resolution",
			"-request",
			param,
		},
	}
	return []*models.Step{step}, nil
}
