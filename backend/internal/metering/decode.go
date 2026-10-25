package metering

import (
	"encoding/json"
	"net/http"

	"github.com/senasd489/ddo-case-sena-sadik/backend/internal/httpx"
)

// httpxDecodeLarge decodes bodies larger than httpx.Decode's 1 MiB limit
// (the caller already wrapped r.Body in http.MaxBytesReader).
func httpxDecodeLarge(r *http.Request, dst any) error {
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return httpx.BadRequest("geçersiz JSON gövdesi: " + err.Error())
	}
	return nil
}
