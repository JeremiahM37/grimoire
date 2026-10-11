package seal

import "testing"

func TestEnvelopeRoundTripCarriesEvidenceAndCheck(t *testing.T) {
	t.Parallel()
	in := Payload{Text: "the release channel is nightly", Evidence: "notes/a.md,https://example.test/x", Check: "ls /srv/channel"}
	plain := EncodeSealed(in)
	out, enveloped := DecodeSealed(plain)
	if !enveloped {
		t.Fatal("an encoded payload did not decode as an envelope")
	}
	if out != in {
		t.Fatalf("round trip = %+v, want %+v", out, in)
	}
}

func TestEnvelopeRoundTripWithNoMetadata(t *testing.T) {
	t.Parallel()
	out, enveloped := DecodeSealed(EncodeSealed(Payload{Text: "plain fact"}))
	if !enveloped || out.Text != "plain fact" || out.Evidence != "" || out.Check != "" {
		t.Fatalf("got %+v enveloped=%v", out, enveloped)
	}
}

// A ciphertext sealed before envelopes is bare text. It must come back as the
// fact, unenveloped, so the caller knows its metadata is not in the payload.
func TestBareTextIsNotAnEnvelope(t *testing.T) {
	t.Parallel()
	for _, bare := range []string{
		"zeppelin-quartz-harbor forecast seven",
		`{"text":"looks like json but has no prefix"}`,
		"",
	} {
		out, enveloped := DecodeSealed(bare)
		if enveloped || out.Text != bare || out.Evidence != "" || out.Check != "" {
			t.Errorf("bare %q decoded as %+v enveloped=%v", bare, out, enveloped)
		}
	}
}

// A legacy fact that happens to start with the prefix but is not a well-formed
// envelope must stay bare text, not be misread.
func TestMalformedEnvelopeFallsBackToBareText(t *testing.T) {
	t.Parallel()
	for _, bad := range []string{
		envelopePrefix + "not json",
		envelopePrefix + `{"text":"x","unexpected":"field"}`,
		envelopePrefix + `{"text":"x"} trailing`,
	} {
		out, enveloped := DecodeSealed(bad)
		if enveloped || out.Text != bad {
			t.Errorf("malformed %q decoded as %+v enveloped=%v", bad, out, enveloped)
		}
	}
}
