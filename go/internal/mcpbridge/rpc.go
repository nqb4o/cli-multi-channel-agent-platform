// rpc.go — JSON-RPC 2.0 type helpers for the MCP loopback bridge.
//
// The actual dispatch logic lives in bridge.go (rpcDispatcher). This file
// provides the canonical request / response structs used when writing tests
// that need to construct well-formed payloads.
package mcpbridge

import "encoding/json"

// RPCRequest is a minimal JSON-RPC 2.0 request envelope.
type RPCRequest struct {
	JSONRPC string         `json:"jsonrpc"`
	ID      any            `json:"id"`
	Method  string         `json:"method"`
	Params  map[string]any `json:"params,omitempty"`
}

// RPCResponse is a minimal JSON-RPC 2.0 response envelope.
type RPCResponse struct {
	JSONRPC string         `json:"jsonrpc"`
	ID      any            `json:"id,omitempty"`
	Result  any            `json:"result,omitempty"`
	Error   *RPCErrorBody  `json:"error,omitempty"`
}

// RPCErrorBody is the error object inside an RPCResponse.
type RPCErrorBody struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// MarshalRequest encodes a request to JSON bytes.
func MarshalRequest(req RPCRequest) ([]byte, error) {
	return json.Marshal(req)
}

// ParseResponse decodes a JSON body into an RPCResponse.
func ParseResponse(body []byte) (RPCResponse, error) {
	var r RPCResponse
	dec := json.NewDecoder(bytesReader(body))
	dec.UseNumber()
	err := dec.Decode(&r)
	return r, err
}

// bytesReader is a tiny helper so we don't need bytes in the import.
type bytesReaderT struct{ b []byte; pos int }
func (r *bytesReaderT) Read(p []byte) (int, error) {
	if r.pos >= len(r.b) {
		return 0, nil
	}
	n := copy(p, r.b[r.pos:])
	r.pos += n
	return n, nil
}
func bytesReader(b []byte) *bytesReaderT { return &bytesReaderT{b: b} }
