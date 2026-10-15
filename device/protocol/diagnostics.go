package protocol

// Diagnostics collection. An admin asks for a device's diagnostics in the console; the health
// response names the oldest request the device has not answered (HealthResponse.DiagnosticsRequest),
// and the device answers it once with POST /v1/diagnostics: a zip of its health snapshot and its
// own logs, over its authenticated connection. The bundle never holds prompt content, keys or the
// spool.
const DiagnosticsUploadPath = "/v1/diagnostics"

// HeaderDiagnosticsRequest names the request an upload answers.
const HeaderDiagnosticsRequest = "X-Sac-Diagnostics-Request"

// DiagnosticsContentType is the upload's media type.
const DiagnosticsContentType = "application/zip"

// MaxDiagnosticsBundleBytes caps one uploaded bundle. Logs compress tenfold or better, and the
// upload has to finish within the device edge's 60-second request timeout.
const MaxDiagnosticsBundleBytes = 16 << 20

// ReasonDiagnosticsNotPending is the refusal of an upload whose request is not waiting for this
// device: unknown, already answered or expired. The device drops it.
const ReasonDiagnosticsNotPending ReasonCode = "diagnostics_not_pending"
