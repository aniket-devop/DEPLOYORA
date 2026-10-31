package domain

// Reason is the machine-readable cause that accompanies an outcome.
type Reason string

const (
	ReasonValidationFailed      Reason = "validation_failed"
	ReasonRenderFailed          Reason = "render_failed"
	ReasonSchemaMissing         Reason = "schema_missing"
	ReasonConfigInvalid         Reason = "config_invalid"
	ReasonPrerequisiteMissing   Reason = "prerequisite_missing"
	ReasonEnvironmentError      Reason = "environment_error"
	ReasonProvisionFailed       Reason = "provision_failed"
	ReasonImageLoadFailed       Reason = "image_load_failed"
	ReasonAdmissionRejected     Reason = "admission_rejected"
	ReasonInvalidWorkloadConfig Reason = "invalid_workload_config"
	ReasonAuthorizationFailed   Reason = "authorization_failed"
	ReasonApplyConflict         Reason = "apply_conflict"
	ReasonWebhookUnavailable    Reason = "webhook_unavailable"
	ReasonDeployAPIError        Reason = "deploy_api_error"
	ReasonAPIUnavailable        Reason = "api_unavailable"
	ReasonClusterError          Reason = "cluster_error"
	ReasonCheckFailed           Reason = "check_failed"
	ReasonCheckTimeout          Reason = "check_timeout"
	ReasonRunDeadline           Reason = "run_deadline"
	ReasonStageDeadline         Reason = "stage_deadline"
	ReasonInterrupted           Reason = "interrupted"
	ReasonInternalError         Reason = "internal_error"
)

// AllReasons returns every reason code.
func AllReasons() []Reason {
	return []Reason{
		ReasonValidationFailed, ReasonRenderFailed, ReasonSchemaMissing, ReasonConfigInvalid,
		ReasonPrerequisiteMissing, ReasonEnvironmentError, ReasonProvisionFailed,
		ReasonImageLoadFailed, ReasonAdmissionRejected, ReasonInvalidWorkloadConfig,
		ReasonAuthorizationFailed, ReasonApplyConflict, ReasonWebhookUnavailable,
		ReasonDeployAPIError, ReasonAPIUnavailable, ReasonClusterError, ReasonCheckFailed,
		ReasonCheckTimeout, ReasonRunDeadline, ReasonStageDeadline, ReasonInterrupted,
		ReasonInternalError,
	}
}

// Warning is a problem recorded without changing the outcome. Wind-down problems
// are always warnings and never become an outcome.
type Warning string

const (
	WarningEvidenceIncomplete      Warning = "evidence_incomplete"
	WarningReportFailed            Warning = "report_failed"
	WarningCleanupFailed           Warning = "cleanup_failed"
	WarningWindDownInterrupted     Warning = "wind_down_interrupted"
	WarningInternalErrorInWindDown Warning = "internal_error_in_wind_down"
)

// AllWarnings returns every warning code.
func AllWarnings() []Warning {
	return []Warning{
		WarningEvidenceIncomplete, WarningReportFailed, WarningCleanupFailed,
		WarningWindDownInterrupted, WarningInternalErrorInWindDown,
	}
}
