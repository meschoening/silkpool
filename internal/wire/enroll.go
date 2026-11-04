package wire

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"

	"github.com/meschoening/silkpool/internal/node"
	"github.com/meschoening/silkpool/internal/pool"
	"github.com/meschoening/silkpool/internal/secret"
	"github.com/meschoening/silkpool/internal/store"
)

// A Code is an enrollment code: the Hub's URL, the pin of its key when
// it serves its own certificate, and a one-time secret. Holding it is the
// approval to enroll one machine, so it's a secret as a whole: only
// Text gives it back.
type Code struct {
	URL string
	// Pin is the Hub's pinned key, or empty for a Hub with a publicly
	// trusted certificate.
	Pin node.Pin
	// Secret is the secret the Hub knows the code by. It keeps only its
	// Hash.
	Secret secret.Value
}

// codeVersion is the first byte of a code's payload, so a later layout
// can tell itself apart.
const codeVersion = 1

// secretSize is the size of a code's secret in bytes: 128 bits, for a
// code that enrolls one machine within 24 hours.
const secretSize = 16

// NewCode returns a new enrollment code for the Hub at hubURL, pinned to
// pin unless it's empty, with a secret drawn at random.
func NewCode(hubURL string, pin node.Pin) (Code, error) {
	if err := checkHubURL(hubURL); err != nil {
		return Code{}, err
	}
	if pin != "" {
		if _, err := pinBytes(pin); err != nil {
			return Code{}, err
		}
	}
	b := make([]byte, secretSize)
	_, _ = rand.Read(b)
	return Code{URL: hubURL, Pin: pin, Secret: secret.New(base64.RawURLEncoding.EncodeToString(b))}, nil
}

// Text returns the code as the operator copies it: secret.CodePrefix,
// then its payload in unpadded base64url, which the log scrubber
// recognizes.
func (c Code) Text() secret.Value {
	raw, _ := base64.RawURLEncoding.DecodeString(c.Secret.Reveal())
	payload := []byte{codeVersion, 0}
	payload = append(payload, raw...)
	if pin, err := pinBytes(c.Pin); err == nil {
		payload[1] = 1
		payload = append(payload, pin...)
	}
	payload = append(payload, c.URL...)
	return secret.New(secret.CodePrefix + base64.RawURLEncoding.EncodeToString(payload))
}

// ErrNotACode refuses text that isn't an enrollment code. It never
// repeats the text, which may be one with a typo.
var ErrNotACode = errors.New("that isn't an enrollment code: copy it again from the Hub")

// ParseCode returns the enrollment code text holds.
func ParseCode(text string) (Code, error) {
	encoded, ok := strings.CutPrefix(strings.TrimSpace(text), secret.CodePrefix)
	if !ok {
		return Code{}, ErrNotACode
	}
	payload, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil || len(payload) < 2+secretSize || payload[0] != codeVersion || payload[1] > 1 {
		return Code{}, ErrNotACode
	}
	c := Code{Secret: secret.New(base64.RawURLEncoding.EncodeToString(payload[2 : 2+secretSize]))}
	rest := payload[2+secretSize:]
	if payload[1] == 1 {
		if len(rest) < sha256.Size {
			return Code{}, ErrNotACode
		}
		c.Pin = node.Pin(pinPrefix + hex.EncodeToString(rest[:sha256.Size]))
		rest = rest[sha256.Size:]
	}
	c.URL = string(rest)
	if checkHubURL(c.URL) != nil {
		return Code{}, ErrNotACode
	}
	return c, nil
}

// checkHubURL refuses a Hub URL that isn't an https:// URL.
func checkHubURL(s string) error {
	u, err := url.Parse(s)
	if err != nil || u.Scheme != "https" || u.Host == "" {
		return fmt.Errorf("%q isn't an https:// URL", s)
	}
	return nil
}

// Hash returns the hash the Hub keeps of a code's secret, and matches an
// enrollment by.
func Hash(s secret.Value) pool.Hash { return sha256.Sum256([]byte(s.Reveal())) }

// Machine is what an enrolling machine tells the Hub about itself.
type Machine struct {
	Hostname     string `json:"hostname"`
	OS           string `json:"os"`
	OSVersion    string `json:"os_version"`
	Arch         string `json:"arch"`
	AgentVersion string `json:"agent_version"`
	// Zone is the OS's time zone, by its IANA name.
	Zone string `json:"zone"`
}

// Enroll is a machine's enrollment, which it POSTs to EnrollPath: its
// code's secret, which the Hub hashes before it applies pool.Enroll, its
// node key's public half, the name it asks for, its protocol version,
// its facts, and the origin of the administrator's enroll, which the
// enrollment's entry in history has.
type Enroll struct {
	Secret   secret.Value
	Key      string
	Name     string
	Protocol int
	Machine  Machine
	Origin   node.Origin
}

type enrollBody struct {
	Secret   string       `json:"secret"`
	Key      string       `json:"key"`
	Name     string       `json:"name"`
	Protocol int          `json:"protocol"`
	Machine  Machine      `json:"machine"`
	Origin   store.Origin `json:"origin,omitzero"`
}

// MarshalEnroll encodes e for the enrollment endpoint. It's the one
// place the code's secret leaves the machine.
func MarshalEnroll(e Enroll) ([]byte, error) {
	origin, err := store.FromOrigin(e.Origin)
	if err != nil {
		return nil, err
	}
	return json.Marshal(enrollBody{Secret: e.Secret.Reveal(), Key: e.Key, Name: e.Name, Protocol: e.Protocol, Machine: e.Machine, Origin: origin})
}

// UnmarshalEnroll decodes an enrollment the endpoint received.
func UnmarshalEnroll(b []byte) (Enroll, error) {
	var body enrollBody
	if err := json.Unmarshal(b, &body); err != nil {
		return Enroll{}, fmt.Errorf("wire: an enrollment that isn't one: %w", err)
	}
	origin, err := body.Origin.ToOrigin()
	if err != nil {
		return Enroll{}, fmt.Errorf("wire: an enrollment with an origin that isn't one: %w", err)
	}
	return Enroll{Secret: secret.New(body.Secret), Key: body.Key, Name: body.Name, Protocol: body.Protocol, Machine: body.Machine, Origin: origin}, nil
}

// An Answer is the Hub's answer to an enrollment: the node's ID, name
// and target, and the Hub's ID, or the rule that refused it, in which
// case nothing was saved.
type Answer struct {
	Node, Name, Target string
	Hub                string
	Refused            error
}

type answerBody struct {
	Node    string   `json:"node,omitempty"`
	Name    string   `json:"name,omitempty"`
	Target  string   `json:"target,omitempty"`
	Hub     string   `json:"hub,omitempty"`
	Refused *refusal `json:"refused,omitempty"`
}

// MarshalAnswer encodes the Hub's answer to an enrollment.
func MarshalAnswer(a Answer) ([]byte, error) {
	body := answerBody{Node: a.Node, Name: a.Name, Target: a.Target, Hub: a.Hub}
	if a.Refused != nil {
		r := fromRefusal(a.Refused)
		body = answerBody{Refused: &r}
	}
	return json.Marshal(body)
}

// UnmarshalAnswer decodes the Hub's answer to an enrollment. A refusal's
// rule comes back as the error the Hub refused with, so errors.Is and
// errors.As find pool's and node's refusals.
func UnmarshalAnswer(b []byte) (Answer, error) {
	var body answerBody
	if err := json.Unmarshal(b, &body); err != nil {
		return Answer{}, fmt.Errorf("wire: an answer that isn't one: %w", err)
	}
	if body.Refused != nil {
		return Answer{Refused: body.Refused.toError()}, nil
	}
	if body.Node == "" || body.Hub == "" {
		return Answer{}, errors.New("wire: an answer with no node or Hub")
	}
	return Answer{Node: body.Node, Name: body.Name, Target: body.Target, Hub: body.Hub}, nil
}
