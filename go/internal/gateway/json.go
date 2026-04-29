package gateway

import (
	"encoding/json"
	"net/http"
)

// writeJSONError writes a uniform `{"error": "<msg>"}` body with the given
// status. Helpers in auth.go and authuser.go call this; centralising the
// shape keeps the gateway's error envelope consistent across routes.
func writeJSONError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}
