// Code generated by go-swagger; DO NOT EDIT.

package installer

// This file was generated by the swagger tool.
// Editing this file might prove futile when you re-run the swagger generate command

import (
	"context"
	"net/http"
	"time"

	"github.com/go-openapi/errors"
	"github.com/go-openapi/runtime"
	cr "github.com/go-openapi/runtime/client"
	"github.com/go-openapi/strfmt"

	"github.com/openshift/assisted-service/models"
)

// NewV2UpdateHostInstallerArgsParams creates a new V2UpdateHostInstallerArgsParams object
// with the default values initialized.
func NewV2UpdateHostInstallerArgsParams() *V2UpdateHostInstallerArgsParams {
	var ()
	return &V2UpdateHostInstallerArgsParams{

		timeout: cr.DefaultTimeout,
	}
}

// NewV2UpdateHostInstallerArgsParamsWithTimeout creates a new V2UpdateHostInstallerArgsParams object
// with the default values initialized, and the ability to set a timeout on a request
func NewV2UpdateHostInstallerArgsParamsWithTimeout(timeout time.Duration) *V2UpdateHostInstallerArgsParams {
	var ()
	return &V2UpdateHostInstallerArgsParams{

		timeout: timeout,
	}
}

// NewV2UpdateHostInstallerArgsParamsWithContext creates a new V2UpdateHostInstallerArgsParams object
// with the default values initialized, and the ability to set a context for a request
func NewV2UpdateHostInstallerArgsParamsWithContext(ctx context.Context) *V2UpdateHostInstallerArgsParams {
	var ()
	return &V2UpdateHostInstallerArgsParams{

		Context: ctx,
	}
}

// NewV2UpdateHostInstallerArgsParamsWithHTTPClient creates a new V2UpdateHostInstallerArgsParams object
// with the default values initialized, and the ability to set a custom HTTPClient for a request
func NewV2UpdateHostInstallerArgsParamsWithHTTPClient(client *http.Client) *V2UpdateHostInstallerArgsParams {
	var ()
	return &V2UpdateHostInstallerArgsParams{
		HTTPClient: client,
	}
}

/*V2UpdateHostInstallerArgsParams contains all the parameters to send to the API endpoint
for the v2 update host installer args operation typically these are written to a http.Request
*/
type V2UpdateHostInstallerArgsParams struct {

	/*HostID
	  The host whose installer arguments should be updated.

	*/
	HostID strfmt.UUID
	/*InfraEnvID
	  The InfraEnv of the host whose installer arguments should be updated.

	*/
	InfraEnvID strfmt.UUID
	/*InstallerArgsParams
	  The updated installer arguments.

	*/
	InstallerArgsParams *models.InstallerArgsParams

	timeout    time.Duration
	Context    context.Context
	HTTPClient *http.Client
}

// WithTimeout adds the timeout to the v2 update host installer args params
func (o *V2UpdateHostInstallerArgsParams) WithTimeout(timeout time.Duration) *V2UpdateHostInstallerArgsParams {
	o.SetTimeout(timeout)
	return o
}

// SetTimeout adds the timeout to the v2 update host installer args params
func (o *V2UpdateHostInstallerArgsParams) SetTimeout(timeout time.Duration) {
	o.timeout = timeout
}

// WithContext adds the context to the v2 update host installer args params
func (o *V2UpdateHostInstallerArgsParams) WithContext(ctx context.Context) *V2UpdateHostInstallerArgsParams {
	o.SetContext(ctx)
	return o
}

// SetContext adds the context to the v2 update host installer args params
func (o *V2UpdateHostInstallerArgsParams) SetContext(ctx context.Context) {
	o.Context = ctx
}

// WithHTTPClient adds the HTTPClient to the v2 update host installer args params
func (o *V2UpdateHostInstallerArgsParams) WithHTTPClient(client *http.Client) *V2UpdateHostInstallerArgsParams {
	o.SetHTTPClient(client)
	return o
}

// SetHTTPClient adds the HTTPClient to the v2 update host installer args params
func (o *V2UpdateHostInstallerArgsParams) SetHTTPClient(client *http.Client) {
	o.HTTPClient = client
}

// WithHostID adds the hostID to the v2 update host installer args params
func (o *V2UpdateHostInstallerArgsParams) WithHostID(hostID strfmt.UUID) *V2UpdateHostInstallerArgsParams {
	o.SetHostID(hostID)
	return o
}

// SetHostID adds the hostId to the v2 update host installer args params
func (o *V2UpdateHostInstallerArgsParams) SetHostID(hostID strfmt.UUID) {
	o.HostID = hostID
}

// WithInfraEnvID adds the infraEnvID to the v2 update host installer args params
func (o *V2UpdateHostInstallerArgsParams) WithInfraEnvID(infraEnvID strfmt.UUID) *V2UpdateHostInstallerArgsParams {
	o.SetInfraEnvID(infraEnvID)
	return o
}

// SetInfraEnvID adds the infraEnvId to the v2 update host installer args params
func (o *V2UpdateHostInstallerArgsParams) SetInfraEnvID(infraEnvID strfmt.UUID) {
	o.InfraEnvID = infraEnvID
}

// WithInstallerArgsParams adds the installerArgsParams to the v2 update host installer args params
func (o *V2UpdateHostInstallerArgsParams) WithInstallerArgsParams(installerArgsParams *models.InstallerArgsParams) *V2UpdateHostInstallerArgsParams {
	o.SetInstallerArgsParams(installerArgsParams)
	return o
}

// SetInstallerArgsParams adds the installerArgsParams to the v2 update host installer args params
func (o *V2UpdateHostInstallerArgsParams) SetInstallerArgsParams(installerArgsParams *models.InstallerArgsParams) {
	o.InstallerArgsParams = installerArgsParams
}

// WriteToRequest writes these params to a swagger request
func (o *V2UpdateHostInstallerArgsParams) WriteToRequest(r runtime.ClientRequest, reg strfmt.Registry) error {

	if err := r.SetTimeout(o.timeout); err != nil {
		return err
	}
	var res []error

	// path param host_id
	if err := r.SetPathParam("host_id", o.HostID.String()); err != nil {
		return err
	}

	// path param infra_env_id
	if err := r.SetPathParam("infra_env_id", o.InfraEnvID.String()); err != nil {
		return err
	}

	if o.InstallerArgsParams != nil {
		if err := r.SetBodyParam(o.InstallerArgsParams); err != nil {
			return err
		}
	}

	if len(res) > 0 {
		return errors.CompositeValidationError(res...)
	}
	return nil
}
