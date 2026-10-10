package plugin

import (
	"encoding/json"
	"fmt"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
)

func okEnvelope(result any) []byte {
	raw, _ := json.Marshal(result)
	out, _ := json.Marshal(pluginabi.Envelope{OK: true, Result: raw})
	return out
}

// ErrEnvelope encodes a failed RPC result. Messages must be secret-free.
func ErrEnvelope(code, message string) []byte {
	out, _ := json.Marshal(pluginabi.Envelope{OK: false, Error: &pluginabi.Error{Code: code, Message: message}})
	return out
}

func decodeEnvelope(raw []byte) (pluginabi.Envelope, error) {
	var env pluginabi.Envelope
	if len(raw) == 0 {
		return env, fmt.Errorf("empty envelope")
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return env, fmt.Errorf("undecodable envelope")
	}
	return env, nil
}
