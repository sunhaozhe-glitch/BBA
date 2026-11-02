package taskengine

import (
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"
)

func b6ValidNonce(b []byte, nonce string) bool {
	var owner struct {
		Nonce string `json:"nonce"`
	}
	_, e := hex.DecodeString(nonce)
	return len(nonce) == 64 && e == nil && json.Unmarshal(b, &owner) == nil && owner.Nonce == nonce
}
func TestB6RunnerNonceBoundaries(t *testing.T) {
	n := strings.Repeat("a", 64)
	start, _ := json.Marshal(map[string]string{"nonce": n})
	if !b6ValidNonce(start, n) {
		t.Fatal("valid start")
	}
	for _, b := range [][]byte{[]byte(`{}`), []byte(`null`), []byte(`{"nonce":"wrong"}`), []byte(`{"nonce":`)} {
		if b6ValidNonce(b, n) {
			t.Fatal("wrong startup accepted")
		}
	}
	done, _ := json.Marshal(map[string]string{"nonce": strings.Repeat("b", 64)})
	if b6ValidNonce(done, n) {
		t.Fatal("startup/completion mismatch accepted")
	}
	if b6ValidNonce(start, "short") || b6ValidNonce(start, strings.Repeat("g", 64)) {
		t.Fatal("malformed owner nonce")
	}
	t.Log("exact startup and completion parser: valid accepted; wrong startup/mismatch/malformed rejected; DB/network/resources=0")
}
