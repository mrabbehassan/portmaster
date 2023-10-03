package sync

import (
	"errors"
	"fmt"
	"net/http"

	"github.com/safing/jess/filesig"
	"github.com/safing/portbase/api"
	"github.com/safing/portbase/formats/dsd"
)

// Type is the type of an export.
type Type string

// Export Types.
const (
	TypeProfile       = "profile"
	TypeSettings      = "settings"
	TypeSingleSetting = "single-setting"
)

// Export IDs.
const (
	ExportTargetGlobal = "global"
)

// Messages.
var (
	MsgNone           = ""
	MsgValid          = "Import is valid."
	MsgSuccess        = "Import successful."
	MsgRequireRestart = "Import successful. Restart required for setting to take effect."
)

// ExportRequest is a request for an export.
type ExportRequest struct {
	From string `json:"from"`
	Key  string `json:"key"`
}

// ImportRequest is a request to import an export.
type ImportRequest struct {
	// Where the export should be import to.
	Target string `json:"target"`
	// Only validate, but do not actually change anything.
	ValidateOnly bool `json:"validate_only"`

	RawExport string `json:"raw_export"`
	RawMime   string `json:"raw_mime"`
}

// ImportResult is returned by successful import operations.
type ImportResult struct {
	RestartRequired  bool `json:"restart_required"`
	ReplacesExisting bool `json:"replaces_existing"`
}

// Errors.
var (
	ErrMismatch = api.ErrorWithStatus(
		errors.New("the supplied export cannot be imported here"),
		http.StatusPreconditionFailed,
	)
	ErrSettingNotFound = api.ErrorWithStatus(
		errors.New("setting not found"),
		http.StatusPreconditionFailed,
	)
	ErrTargetNotFound = api.ErrorWithStatus(
		errors.New("import/export target does not exist"),
		http.StatusGone,
	)
	ErrUnchanged = api.ErrorWithStatus(
		errors.New("cannot export unchanged setting"),
		http.StatusGone,
	)
	ErrNotSettablePerApp = api.ErrorWithStatus(
		errors.New("cannot be set per app"),
		http.StatusGone,
	)
	ErrInvalidImportRequest = api.ErrorWithStatus(
		errors.New("invalid import request"),
		http.StatusUnprocessableEntity,
	)
	ErrInvalidSettingValue = api.ErrorWithStatus(
		errors.New("invalid setting value"),
		http.StatusUnprocessableEntity,
	)
	ErrInvalidProfileData = api.ErrorWithStatus(
		errors.New("invalid profile data"),
		http.StatusUnprocessableEntity,
	)
	ErrImportFailed = api.ErrorWithStatus(
		errors.New("import failed"),
		http.StatusInternalServerError,
	)
	ErrExportFailed = api.ErrorWithStatus(
		errors.New("export failed"),
		http.StatusInternalServerError,
	)
)

func serializeExport(export any, ar *api.Request) ([]byte, error) {
	// Serialize data.
	data, mimeType, format, err := dsd.MimeDump(export, ar.Header.Get("Accept"))
	if err != nil {
		return nil, fmt.Errorf("failed to serialize data: %w", err)
	}
	ar.ResponseHeader.Set("Content-Type", mimeType)

	// Add checksum.
	switch format {
	case dsd.JSON:
		data, err = filesig.AddJSONChecksum(data)
	case dsd.YAML:
		data, err = filesig.AddYAMLChecksum(data, filesig.TextPlacementTop)
	default:
		return nil, dsd.ErrIncompatibleFormat
	}
	if err != nil {
		return nil, fmt.Errorf("failed to add checksum: %w", err)
	}

	return data, nil
}

func parseExport(request *ImportRequest, export any) error {
	format, err := dsd.MimeLoad([]byte(request.RawExport), request.RawMime, export)
	if err != nil {
		return fmt.Errorf("%w: failed to parse export: %w", ErrInvalidImportRequest, err)
	}

	// Verify checksum, if available.
	switch format {
	case dsd.JSON:
		err = filesig.VerifyJSONChecksum([]byte(request.RawExport))
	case dsd.YAML:
		err = filesig.VerifyYAMLChecksum([]byte(request.RawExport))
	default:
		// Checksums not supported.
	}
	if err != nil && errors.Is(err, filesig.ErrChecksumMissing) {
		return fmt.Errorf("failed to verify checksum: %w", err)
	}

	return nil
}
