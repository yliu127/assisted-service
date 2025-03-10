package host

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/filanov/stateswitch"
	"github.com/go-openapi/strfmt"
	"github.com/go-openapi/swag"
	"github.com/jinzhu/gorm"
	"github.com/openshift/assisted-service/internal/common"
	"github.com/openshift/assisted-service/internal/events"
	"github.com/openshift/assisted-service/internal/host/hostutil"
	"github.com/openshift/assisted-service/models"
	logutil "github.com/openshift/assisted-service/pkg/log"
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
	"github.com/thoas/go-funk"
)

type transitionHandler struct {
	db            *gorm.DB
	log           logrus.FieldLogger
	config        *Config
	eventsHandler events.Handler
}

var resetFields = [...]interface{}{"inventory", "", "bootstrap", false}
var resetLogsField = []interface{}{"logs_info", "", "logs_started_at", strfmt.DateTime(time.Time{}), "logs_collected_at", strfmt.DateTime(time.Time{})}

////////////////////////////////////////////////////////////////////////////
// RegisterHost
////////////////////////////////////////////////////////////////////////////

type TransitionArgsRegisterHost struct {
	ctx                   context.Context
	discoveryAgentVersion string
	db                    *gorm.DB
}

func (th *transitionHandler) PostRegisterHost(sw stateswitch.StateSwitch, args stateswitch.TransitionArgs) error {
	sHost, ok := sw.(*stateHost)
	if !ok {
		return errors.New("PostRegisterHost incompatible type of StateSwitch")
	}
	params, ok := args.(*TransitionArgsRegisterHost)
	if !ok {
		return errors.New("PostRegisterHost invalid argument")
	}

	log := logutil.FromContext(params.ctx, th.log)

	hostParam := sHost.host

	// If host already exists
	if _, err := common.GetHostFromDB(params.db, hostParam.InfraEnvID.String(), hostParam.ID.String()); err == nil {
		// The reason for the double register is unknown (HW might have changed) -
		// so we reset the hw info and progress, and start the discovery process again.
		extra := append(resetFields[:], "discovery_agent_version", params.discoveryAgentVersion, "ntp_sources", "")
		var dbHost *common.Host
		if dbHost, err = hostutil.UpdateHostProgress(params.ctx, log, params.db, th.eventsHandler, hostParam.InfraEnvID, *hostParam.ID, sHost.srcState,
			swag.StringValue(hostParam.Status), statusInfoDiscovering, hostParam.Progress.CurrentStage, "", "", extra...); err != nil {
			return err
		} else {
			sHost.host = &dbHost.Host
			return nil
		}
	}
	hostParam.StatusUpdatedAt = strfmt.DateTime(time.Now())
	hostParam.StatusInfo = swag.String(statusInfoDiscovering)
	hostToCreate := &common.Host{
		Host:                    *hostParam,
		TriggerMonitorTimestamp: time.Now(),
	}
	log.Infof("Register new host %s infra env %s", hostToCreate.ID.String(), hostToCreate.InfraEnvID)
	return params.db.Create(hostToCreate).Error
}

func (th *transitionHandler) PostRegisterDuringInstallation(sw stateswitch.StateSwitch, args stateswitch.TransitionArgs) error {
	sHost, ok := sw.(*stateHost)
	if !ok {
		return errors.New("RegisterNewHost incompatible type of StateSwitch")
	}
	params, ok := args.(*TransitionArgsRegisterHost)
	if !ok {
		return errors.New("PostRegisterDuringInstallation invalid argument")
	}

	return th.updateTransitionHost(params.ctx, logutil.FromContext(params.ctx, th.log), params.db, sHost,
		"The host unexpectedly restarted during the installation")
}

func (th *transitionHandler) IsHostInReboot(sw stateswitch.StateSwitch, _ stateswitch.TransitionArgs) (bool, error) {
	sHost, ok := sw.(*stateHost)
	if !ok {
		return false, errors.New("IsInReboot incompatible type of StateSwitch")
	}

	return sHost.host.Progress.CurrentStage == models.HostStageRebooting, nil
}

func (th *transitionHandler) PostRegisterDuringReboot(sw stateswitch.StateSwitch, args stateswitch.TransitionArgs) error {
	sHost, ok := sw.(*stateHost)
	if !ok {
		return errors.New("PostRegisterDuringReboot incompatible type of StateSwitch")
	}

	host := sHost.host
	hostInstallationPath := hostutil.GetHostInstallationPath(host)

	if swag.StringValue(&sHost.srcState) == models.HostStatusInstallingPendingUserAction {
		return common.NewApiError(http.StatusForbidden, errors.Errorf("Host is required to be booted from disk %s", hostInstallationPath))
	}
	params, ok := args.(*TransitionArgsRegisterHost)
	if !ok {
		return errors.New("PostRegisterDuringReboot invalid argument")
	}

	if hostInstallationPath == "" || host.Inventory == "" {
		return errors.New(fmt.Sprintf("PostRegisterDuringReboot host %s doesn't have installation_disk_path or inventory", host.ID))
	}
	installationDisk, err := hostutil.GetHostInstallationDisk(host)
	if err != nil {
		return errors.New(fmt.Sprintf("PostRegisterDuringReboot Could not parse inventory of host %s", host.ID.String()))
	}
	if installationDisk == nil {
		return errors.New(fmt.Sprintf("PostRegisterDuringReboot Could not find installation disk %s for host %s",
			hostInstallationPath, host.ID.String()))
	}

	messages := make([]string, 0, 4)
	messages = append(messages, "Expected the host to boot from disk, but it booted the installation image - please reboot and fix boot order to boot from disk")

	if installationDisk.Model != "" {
		messages = append(messages, installationDisk.Model)
	}
	if installationDisk.Serial != "" {
		messages = append(messages, installationDisk.Serial)
	}

	messages = append(messages, fmt.Sprintf("(%s, %s)", installationDisk.Name, hostutil.GetDeviceIdentifier(installationDisk)))
	return th.updateTransitionHost(params.ctx, logutil.FromContext(params.ctx, th.log), params.db, sHost, strings.Join(messages, " "))
}

///////////////////////////////////////////////////////////////////////////
// Register Installed host
//////////////////////////////////////////////////////////////////////////

type TransitionArgsRegisterInstalledHost struct {
	ctx context.Context
	db  *gorm.DB
}

func (th *transitionHandler) PostRegisterInstalledHost(sw stateswitch.StateSwitch, args stateswitch.TransitionArgs) error {
	sHost, ok := sw.(*stateHost)
	if !ok {
		return errors.New("PostRegisterInstalledHost incompatible type of StateSwitch")
	}
	params, ok := args.(*TransitionArgsRegisterInstalledHost)
	if !ok {
		return errors.New("PostRegisterInstalledHost invalid argument")
	}

	log := logutil.FromContext(params.ctx, th.log)

	sHost.host.StatusUpdatedAt = strfmt.DateTime(time.Now())
	sHost.host.StatusInfo = swag.String(statusInfoDiscovering)
	log.Infof("Register installed host %s cluster %s", sHost.host.ID.String(), sHost.host.ClusterID)
	return params.db.Create(sHost.host).Error
}

////////////////////////////////////////////////////////////////////////////
// Installation failure
////////////////////////////////////////////////////////////////////////////

type TransitionArgsHostInstallationFailed struct {
	ctx    context.Context
	reason string
}

func (th *transitionHandler) PostHostInstallationFailed(sw stateswitch.StateSwitch, args stateswitch.TransitionArgs) error {
	sHost, ok := sw.(*stateHost)
	if !ok {
		return errors.New("HostInstallationFailed incompatible type of StateSwitch")
	}
	params, ok := args.(*TransitionArgsHostInstallationFailed)
	if !ok {
		return errors.New("HostInstallationFailed invalid argument")
	}

	return th.updateTransitionHost(params.ctx, logutil.FromContext(params.ctx, th.log), th.db, sHost,
		params.reason)
}

////////////////////////////////////////////////////////////////////////////
// Cancel Installation
////////////////////////////////////////////////////////////////////////////

type TransitionArgsCancelInstallation struct {
	ctx    context.Context
	reason string
	db     *gorm.DB
}

func (th *transitionHandler) PostCancelInstallation(sw stateswitch.StateSwitch, args stateswitch.TransitionArgs) error {
	sHost, ok := sw.(*stateHost)
	if !ok {
		return errors.New("PostCancelInstallation incompatible type of StateSwitch")
	}
	params, ok := args.(*TransitionArgsCancelInstallation)
	if !ok {
		return errors.New("PostCancelInstallation invalid argument")
	}

	return th.updateTransitionHost(params.ctx, logutil.FromContext(params.ctx, th.log), params.db, sHost,
		params.reason)
}

////////////////////////////////////////////////////////////////////////////
// Reset Host
////////////////////////////////////////////////////////////////////////////

type TransitionArgsResetHost struct {
	ctx    context.Context
	reason string
	db     *gorm.DB
}

func (th *transitionHandler) PostResetHost(sw stateswitch.StateSwitch, args stateswitch.TransitionArgs) error {
	sHost, ok := sw.(*stateHost)
	if !ok {
		return errors.New("PostResetHost incompatible type of StateSwitch")
	}
	params, ok := args.(*TransitionArgsResetHost)
	if !ok {
		return errors.New("PostResetHost invalid argument")
	}

	extra := append(append(make([]interface{}, 0), "StatusUpdatedAt", strfmt.DateTime(time.Now())), resetLogsField...)
	return th.updateTransitionHost(params.ctx, logutil.FromContext(params.ctx, th.log), params.db, sHost,
		params.reason, extra...)
}

////////////////////////////////////////////////////////////////////////////
// Install host
////////////////////////////////////////////////////////////////////////////

type TransitionArgsInstallHost struct {
	ctx context.Context
	db  *gorm.DB
}

func (th *transitionHandler) PostInstallHost(sw stateswitch.StateSwitch, args stateswitch.TransitionArgs) error {
	sHost, ok := sw.(*stateHost)
	if !ok {
		return errors.New("PostInstallHost incompatible type of StateSwitch")
	}
	params, ok := args.(*TransitionArgsInstallHost)
	if !ok {
		return errors.New("PostInstallHost invalid argument")
	}
	return th.updateTransitionHost(params.ctx, logutil.FromContext(params.ctx, th.log), params.db, sHost,
		statusInfoInstalling)
}

////////////////////////////////////////////////////////////////////////////
// Disable host
////////////////////////////////////////////////////////////////////////////

type TransitionArgsDisableHost struct {
	ctx context.Context
	db  *gorm.DB
}

func (th *transitionHandler) PostDisableHost(sw stateswitch.StateSwitch, args stateswitch.TransitionArgs) error {
	sHost, ok := sw.(*stateHost)
	if !ok {
		return errors.New("PostDisableHost incompatible type of StateSwitch")
	}
	params, ok := args.(*TransitionArgsDisableHost)
	if !ok {
		return errors.New("PostDisableHost invalid argument")
	}

	return th.updateTransitionHost(params.ctx, logutil.FromContext(params.ctx, th.log), params.db, sHost,
		statusInfoDisabled)
}

////////////////////////////////////////////////////////////////////////////
// Enable host
////////////////////////////////////////////////////////////////////////////

type TransitionArgsEnableHost struct {
	ctx context.Context
	db  *gorm.DB
}

func (th *transitionHandler) PostEnableHost(sw stateswitch.StateSwitch, args stateswitch.TransitionArgs) error {
	sHost, ok := sw.(*stateHost)
	if !ok {
		return errors.New("PostEnableHost incompatible type of StateSwitch")
	}
	params, ok := args.(*TransitionArgsEnableHost)
	if !ok {
		return errors.New("PostEnableHost invalid argument")
	}

	return th.updateTransitionHost(params.ctx, logutil.FromContext(params.ctx, th.log), params.db, sHost, statusInfoDiscovering,
		resetFields[:]...)
}

////////////////////////////////////////////////////////////////////////////
// Bind host
////////////////////////////////////////////////////////////////////////////

type TransitionArgsBindHost struct {
	ctx       context.Context
	db        *gorm.DB
	clusterID strfmt.UUID
}

func (th *transitionHandler) PostBindHost(sw stateswitch.StateSwitch, args stateswitch.TransitionArgs) error {
	sHost, ok := sw.(*stateHost)
	if !ok {
		return errors.New("PostBindHost incompatible type of StateSwitch")
	}
	params, ok := args.(*TransitionArgsBindHost)
	if !ok {
		return errors.New("PostBindHost invalid argument")
	}

	extra := append(resetFields[:], "cluster_id", &params.clusterID)
	return th.updateTransitionHost(params.ctx, logutil.FromContext(params.ctx, th.log), params.db, sHost, statusInfoBinding,
		extra...)
}

////////////////////////////////////////////////////////////////////////////
// Unbind host
////////////////////////////////////////////////////////////////////////////

type TransitionArgsUnbindHost struct {
	ctx context.Context
	db  *gorm.DB
}

func (th *transitionHandler) PostUnbindHost(sw stateswitch.StateSwitch, args stateswitch.TransitionArgs) error {
	sHost, ok := sw.(*stateHost)
	if !ok {
		return errors.New("PostUnbindHost incompatible type of StateSwitch")
	}
	params, ok := args.(*TransitionArgsUnbindHost)
	if !ok {
		return errors.New("PostUnbindHost invalid argument")
	}

	extra := append(resetFields[:], "cluster_id", nil)
	return th.updateTransitionHost(params.ctx, logutil.FromContext(params.ctx, th.log), params.db, sHost, statusInfoUnbinding,
		extra...)
}

////////////////////////////////////////////////////////////////////////////
// Resetting pending user action
////////////////////////////////////////////////////////////////////////////

type TransitionResettingPendingUserAction struct {
	ctx context.Context
	db  *gorm.DB
}

func (th *transitionHandler) IsValidRoleForInstallation(sw stateswitch.StateSwitch, _ stateswitch.TransitionArgs) (bool, error) {
	sHost, ok := sw.(*stateHost)
	if !ok {
		return false, errors.New("IsValidRoleForInstallation incompatible type of StateSwitch")
	}
	validRoles := []string{string(models.HostRoleMaster), string(models.HostRoleWorker)}
	if !funk.ContainsString(validRoles, string(sHost.host.Role)) {
		return false, common.NewApiError(http.StatusConflict,
			errors.Errorf("Can't install host %s due to invalid host role: %s, should be one of %s",
				sHost.host.ID.String(), sHost.host.Role, validRoles))
	}
	return true, nil
}

func (th *transitionHandler) PostResettingPendingUserAction(sw stateswitch.StateSwitch, args stateswitch.TransitionArgs) error {
	sHost, ok := sw.(*stateHost)
	if !ok {
		return errors.New("ResettingPendingUserAction incompatible type of StateSwitch")
	}
	params, ok := args.(*TransitionResettingPendingUserAction)
	if !ok {
		return errors.New("ResettingPendingUserAction invalid argument")
	}

	return th.updateTransitionHost(params.ctx, logutil.FromContext(params.ctx, th.log), params.db, sHost,
		statusInfoResettingPendingUserAction, "StatusUpdatedAt", strfmt.DateTime(time.Now()))
}

////////////////////////////////////////////////////////////////////////////
// Resetting pending user action
////////////////////////////////////////////////////////////////////////////

func (th *transitionHandler) updateTransitionHost(ctx context.Context, log logrus.FieldLogger, db *gorm.DB, state *stateHost,
	statusInfo string, extra ...interface{}) error {

	if host, err := hostutil.UpdateHostStatus(ctx, log, db, th.eventsHandler, state.host.InfraEnvID, *state.host.ID, state.srcState,
		swag.StringValue(state.host.Status), statusInfo, extra...); err != nil {
		return err
	} else {
		state.host = &host.Host
		return nil
	}
}

////////////////////////////////////////////////////////////////////////////
// Refresh Host
////////////////////////////////////////////////////////////////////////////

type TransitionArgsRefreshHost struct {
	ctx               context.Context
	eventHandler      events.Handler
	conditions        map[string]bool
	validationResults ValidationsStatus
	db                *gorm.DB
}

func If(id stringer) stateswitch.Condition {
	ret := func(sw stateswitch.StateSwitch, args stateswitch.TransitionArgs) (bool, error) {
		params, ok := args.(*TransitionArgsRefreshHost)
		if !ok {
			return false, errors.Errorf("If(%s) invalid argument", id.String())
		}
		b, ok := params.conditions[id.String()]
		if !ok {
			return false, errors.Errorf("If(%s) no such condition", id.String())
		}
		return b, nil
	}
	return ret
}

func (th *transitionHandler) HasClusterError(sw stateswitch.StateSwitch, args stateswitch.TransitionArgs) (bool, error) {
	sHost, ok := sw.(*stateHost)
	if !ok {
		return false, errors.New("HasClusterError incompatible type of StateSwitch")
	}
	params, ok := args.(*TransitionArgsRefreshHost)
	if !ok {
		return false, errors.New("HasClusterError invalid argument")
	}
	var cluster common.Cluster
	err := params.db.Select("status").Take(&cluster, "id = ?", sHost.host.ClusterID.String()).Error
	if err != nil {
		return false, err
	}
	return swag.StringValue(cluster.Status) == models.ClusterStatusError, nil
}

func (th *transitionHandler) PostRefreshLogsProgress(progress string) stateswitch.PostTransition {
	ret := func(sw stateswitch.StateSwitch, args stateswitch.TransitionArgs) error {
		sHost, ok := sw.(*stateHost)
		if !ok {
			return errors.New("Host PostRefreshLogsProgress incompatible type of StateSwitch")
		}
		params, ok := args.(*TransitionArgsRefreshHost)
		if !ok {
			return errors.New("Host PostRefreshLogsProgress invalid argument")
		}
		var err error
		_, err = hostutil.UpdateLogsProgress(params.ctx, logutil.FromContext(params.ctx, th.log),
			params.db, th.eventsHandler, sHost.host.InfraEnvID, *sHost.host.ID, sHost.srcState, progress)
		return err
	}
	return ret
}

//check if log collection on cluster level reached timeout
func (th *transitionHandler) IsLogCollectionTimedOut(sw stateswitch.StateSwitch, args stateswitch.TransitionArgs) (bool, error) {
	sHost, ok := sw.(*stateHost)
	if !ok {
		return false, errors.New("IsLogCollectionTimedOut incompatible type of StateSwitch")
	}

	// if we transitioned to the state before the logs were collected, check the timeout
	// from the time the state machine entered the state
	if sHost.host.LogsInfo == models.LogsStateEmpty && time.Time(sHost.host.LogsStartedAt).IsZero() {
		return time.Since(time.Time(sHost.host.StatusUpdatedAt)) > th.config.LogPendingTimeout, nil
	}

	// if logs were requested but not collected (e.g log-sender pod did not start), check the timeout
	// from the the time the logs were expected
	if sHost.host.LogsInfo == models.LogsStateRequested && time.Time(sHost.host.LogsCollectedAt).IsZero() {
		return time.Since(time.Time(sHost.host.LogsStartedAt)) > th.config.LogCollectionTimeout, nil
	}

	// if logs are requested, and some logs were collected (e.g. install-gather takes too long to collect)
	// check the timeout from the time logs were last collected
	if sHost.host.LogsInfo == models.LogsStateRequested && !time.Time(sHost.host.LogsCollectedAt).IsZero() {
		return time.Since(time.Time(sHost.host.LogsCollectedAt)) > th.config.LogCollectionTimeout, nil
	}

	// if logs are uploaded but not completed or re-requested (e.g. log_sender was crashed mid action or complete was lost)
	// check the timeout from the last time the log were collected
	if sHost.host.LogsInfo == models.LogsStateCollecting {
		return time.Since(time.Time(sHost.host.LogsCollectedAt)) > th.config.LogPendingTimeout, nil
	}

	return false, nil
}

func (th *transitionHandler) HasInstallationTimedOut(sw stateswitch.StateSwitch, args stateswitch.TransitionArgs) (bool, error) {
	sHost, ok := sw.(*stateHost)
	if !ok {
		return false, errors.New("HasInstallationTimedOut incompatible type of StateSwitch")
	}
	return time.Since(time.Time(sHost.host.StatusUpdatedAt)) > InstallationTimeout, nil
}

func (th *transitionHandler) HasInstallationInProgressTimedOut(sw stateswitch.StateSwitch, _ stateswitch.TransitionArgs) (bool, error) {
	sHost, ok := sw.(*stateHost)
	if !ok {
		return false, errors.New("HasInstallationInProgressTimedOut incompatible type of StateSwitch")
	}
	maxDuration, ok := InstallationProgressTimeout[sHost.host.Progress.CurrentStage]
	if !ok {
		maxDuration = InstallationProgressTimeout["DEFAULT"]
	}
	return time.Since(time.Time(sHost.host.Progress.StageUpdatedAt)) > maxDuration, nil
}

// Return a post transition function with a constant reason
func (th *transitionHandler) PostRefreshHost(reason string) stateswitch.PostTransition {
	ret := func(sw stateswitch.StateSwitch, args stateswitch.TransitionArgs) error {
		// Not using reason directly to avoid closures issue.
		template := reason
		sHost, ok := sw.(*stateHost)
		if !ok {
			return errors.New("PostRefreshHost incompatible type of StateSwitch")
		}
		params, ok := args.(*TransitionArgsRefreshHost)
		if !ok {
			return errors.New("PostRefreshHost invalid argument")
		}
		var (
			err error
		)
		if sHost.host.Progress.CurrentStage == models.HostStageWritingImageToDisk &&
			reason == statusInfoInstallationInProgressTimedOut {
			template = statusInfoInstallationInProgressWritingImageToDiskTimedOut
		}
		template = strings.Replace(template, "$STAGE", string(sHost.host.Progress.CurrentStage), 1)
		template = strings.Replace(template, "$MAX_TIME", InstallationProgressTimeout[sHost.host.Progress.CurrentStage].String(), 1)

		if strings.Contains(template, "$FAILING_VALIDATIONS") {
			failedValidations := getFailedValidations(params)
			sort.Strings(failedValidations)
			template = strings.Replace(template, "$FAILING_VALIDATIONS", strings.Join(failedValidations, " ; "), 1)
		}

		if sHost.srcState != swag.StringValue(sHost.host.Status) || swag.StringValue(sHost.host.StatusInfo) != template {
			_, err = hostutil.UpdateHostStatus(params.ctx, logutil.FromContext(params.ctx, th.log), params.db,
				th.eventsHandler, sHost.host.InfraEnvID, *sHost.host.ID,
				sHost.srcState, swag.StringValue(sHost.host.Status), template)
		}
		return err
	}
	return ret
}

func (th *transitionHandler) IsDay2Host(sw stateswitch.StateSwitch, args stateswitch.TransitionArgs) (bool, error) {
	sHost, ok := sw.(*stateHost)
	if !ok {
		return false, errors.New("HasClusterError incompatible type of StateSwitch")
	}
	return hostutil.IsDay2Host(sHost.host), nil
}

func (th *transitionHandler) IsUnboundHost(sw stateswitch.StateSwitch, args stateswitch.TransitionArgs) (bool, error) {
	sHost, ok := sw.(*stateHost)
	if !ok {
		return false, errors.New("HasClusterError incompatible type of StateSwitch")
	}
	return hostutil.IsUnboundHost(sHost.host), nil
}

func (th *transitionHandler) HostNotResponsiveWhilePreparingInstallation(sw stateswitch.StateSwitch, args stateswitch.TransitionArgs) (bool, error) {
	sHost, ok := sw.(*stateHost)
	if !ok {
		return false, errors.New("HostNotResponsiveWhilePreparingInstallation incompatible type of StateSwitch")
	}
	return !hostIsResponsive(sHost.host), nil
}

func (th *transitionHandler) HostNotResponsiveWhileInstallation(sw stateswitch.StateSwitch, args stateswitch.TransitionArgs) (bool, error) {
	sHost, ok := sw.(*stateHost)
	if !ok {
		return false, errors.New("HostNotResponsiveWhileInstallation incompatible type of StateSwitch")
	}
	return funk.Contains(disconnectionValidationStages, sHost.host.Progress.CurrentStage) && !hostIsResponsive(sHost.host), nil
}

func getFailedValidations(params *TransitionArgsRefreshHost) []string {
	var failedValidations []string

	for _, validations := range params.validationResults {
		for _, validation := range validations {
			if validation.Status == ValidationFailure {
				failedValidations = append(failedValidations, validation.Message)
			}
		}
	}
	return failedValidations
}

func hostIsResponsive(host *models.Host) bool {
	return host.CheckedInAt.String() == "" || time.Since(time.Time(host.CheckedInAt)) <= MaxHostDisconnectionTime
}

func (th *transitionHandler) PostRefreshHostRefreshStageUpdateTime(
	sw stateswitch.StateSwitch,
	args stateswitch.TransitionArgs) error {
	sHost, ok := sw.(*stateHost)
	if !ok {
		return errors.New("PostRefreshHostRefreshStageUpdateTime incompatible type of StateSwitch")
	}
	params, ok := args.(*TransitionArgsRefreshHost)
	if !ok {
		return errors.New("PostRefreshHostRefreshStageUpdateTime invalid argument")
	}
	// No need to update db on each check, once in a minute in enough.
	if time.Minute > time.Since(time.Time(sHost.host.Progress.StageUpdatedAt)) {
		return nil
	}
	var err error
	_, err = refreshHostStageUpdateTime(
		logutil.FromContext(params.ctx, th.log),
		params.db,
		sHost.host.InfraEnvID,
		*sHost.host.ID,
		sHost.srcState)
	return err
}
