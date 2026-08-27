package plugin

import (
	"encoding/base64"
	"strings"
	"testing"
)

func TestSnapshotEnvelopePreservesExactRawBytesAndVersion(t *testing.T) {
	for _, raw := range [][]byte{
		[]byte("a: 1\r\nb: 2\r\n"),
		[]byte("a: 1\nb: 2"),
	} {
		snapshot := NewConfigSnapshot(raw)
		decoded, err := snapshot.Decode()
		if err != nil {
			t.Fatal(err)
		}
		if string(decoded) != string(raw) {
			t.Fatalf("decoded=%q want=%q", decoded, raw)
		}
		if snapshot.Version != configVersion(raw) {
			t.Fatalf("version=%q want=%q", snapshot.Version, configVersion(raw))
		}
		if strings.ToLower(snapshot.Version) != snapshot.Version || len(snapshot.Version) != 64 {
			t.Fatalf("invalid version %q", snapshot.Version)
		}
	}
}

func TestCommitProposalValidationRejectsMalformedProtocol(t *testing.T) {
	version := configVersion([]byte("x"))
	valid := CommitProposal{BaseVersion: version, ConfigBase64: base64.StdEncoding.EncodeToString([]byte("a: 1\n"))}
	if _, err := valid.Decode(version); err != nil {
		t.Fatal(err)
	}
	for name, proposal := range map[string]CommitProposal{
		"bad version":   {BaseVersion: "ABC", ConfigBase64: valid.ConfigBase64},
		"wrong version": {BaseVersion: configVersion([]byte("y")), ConfigBase64: valid.ConfigBase64},
		"bad base64":    {BaseVersion: version, ConfigBase64: "***"},
		"missing yaml":  {BaseVersion: version},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := proposal.Decode(version); err == nil {
				t.Fatal("expected rejection")
			}
		})
	}
}
