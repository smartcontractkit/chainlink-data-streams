// config is a separate package so that we can validate
// the config in other packages, for example in job at job create time.

package pluginconfig

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"sort"

	"github.com/ethereum/go-ethereum/common"

	"github.com/smartcontractkit/chainlink-common/keystore/corekeys"
	llotypes "github.com/smartcontractkit/chainlink-common/pkg/types/llo"
	"github.com/smartcontractkit/chainlink-common/pkg/utils/hex"

	mercuryconfig "github.com/smartcontractkit/chainlink-data-streams/mercury/config"
)

type PluginConfig struct {
	ChannelDefinitionsContractAddress   common.Address `json:"channelDefinitionsContractAddress" toml:"channelDefinitionsContractAddress"`
	ChannelDefinitionsContractFromBlock int64          `json:"channelDefinitionsContractFromBlock" toml:"channelDefinitionsContractFromBlock"`

	// NOTE: ChannelDefinitions is an override.
	// If ChannelDefinitions is specified, values for
	// ChannelDefinitionsContractAddress and
	// ChannelDefinitionsContractFromBlock will be ignored
	ChannelDefinitions string `json:"channelDefinitions" toml:"channelDefinitions"`

	// BenchmarkMode is a flag to enable benchmarking mode. In this mode, the
	// transmitter will not transmit anything at all and instead emit
	// logs/metrics.
	BenchmarkMode bool `json:"benchmarkMode" toml:"benchmarkMode"`

	// KeyBundleIDs maps supported keys to their respective bundle IDs
	// Key must match llo's ReportFormat
	KeyBundleIDs map[string]string `json:"keyBundleIDs" toml:"keyBundleIDs"`

	DonID uint32 `json:"donID" toml:"donID"`

	// Mercury servers
	Servers map[string]hex.PlainHexBytes `json:"servers" toml:"servers"`

	Transmitters []TransmitterConfig `json:"transmitters" toml:"transmitters"`

	// PluginVersion selects the LLO plugin the job runs on:
	//   "" or "v30" => llo/v30, on libocr OCR3.0
	//   "v31"       => llo/dev/v31, on libocr OCR3.1
	// NOTE: this names the plugin package, not the LLO offchain ProtocolVersion
	// (0/1/2) carried in the offchain config, nor the ocr2 job spec's own
	// OCRVersion.
	//
	// It applies to every protocol instance in the job. To run the instances on
	// different plugins, which is what a v30 -> v31 blue/green handover needs,
	// use PluginVersions instead.
	PluginVersion string `json:"pluginVersion" toml:"pluginVersion"`

	// PluginVersions selects the plugin per protocol instance, positionally
	// aligned with the job's contract config trackers (index 0 is "Blue", index
	// 1 is "Green"). Entries take the same values as PluginVersion.
	//
	// This exists for the blue/green handover between the v30 and v31 plugins,
	// where the two instances of one job deliberately run different versions:
	//
	//	"pluginVersions": ["v30", "v31"]
	//
	// When empty, every instance falls back to the scalar PluginVersion, so job
	// specs predating this field are unaffected. When non-empty it takes
	// precedence, and its length must match the number of trackers: that check
	// belongs to the consumer, which is the only side that knows how many there
	// are. Validate here only bounds the length and checks each entry.
	PluginVersions []string `json:"pluginVersions" toml:"pluginVersions"`

	// V31 carries the v31 plugin knobs. Only read when the job runs any v31
	// instance.
	V31 V31Config `json:"v31" toml:"v31"`
}

const (
	PluginVersionV30 = "v30"
	PluginVersionV31 = "v31"
)

// MaxProtocolInstances is the number of protocol instances one LLO job may run:
// one production instance, plus one staging instance during a blue/green
// handover. It bounds PluginVersions.
const MaxProtocolInstances = 2

// IsV31 reports whether the job should run on the v31 (libocr OCR3.1) plugin.
//
// NOTE: this reads the scalar PluginVersion only, so it is wrong for a job with
// a mixed PluginVersions list. Prefer IsV31Instance for selecting a plugin and
// AnyV31 for deciding whether to build the OCR3.1-only dependencies. It is kept
// for consumers that have not moved to the per-instance accessors yet.
func (p PluginConfig) IsV31() bool {
	return p.PluginVersion == PluginVersionV31
}

// PluginVersionForInstance returns the plugin version for protocol instance i,
// normalized so that the empty value is reported as PluginVersionV30.
//
// It falls back to the scalar PluginVersion when PluginVersions is empty or
// does not reach index i. A short list is not an error here: Validate bounds
// its length, and matching it against the actual tracker count is the
// consumer's job.
func (p PluginConfig) PluginVersionForInstance(i int) string {
	v := p.PluginVersion
	if i >= 0 && i < len(p.PluginVersions) {
		v = p.PluginVersions[i]
	}
	if v == "" {
		return PluginVersionV30
	}
	return v
}

// IsV31Instance reports whether protocol instance i runs on the v31
// (libocr OCR3.1) plugin.
func (p PluginConfig) IsV31Instance(i int) bool {
	return p.PluginVersionForInstance(i) == PluginVersionV31
}

// AnyV31 reports whether any protocol instance runs on v31.
//
// This is the predicate for building the OCR3.1-only dependencies (the 3.1
// network endpoint factory, the key-value database factory): during a handover
// only one of the two instances is v31, but the dependencies are per job.
func (p PluginConfig) AnyV31() bool {
	if len(p.PluginVersions) == 0 {
		return p.PluginVersion == PluginVersionV31
	}
	for _, v := range p.PluginVersions {
		if v == PluginVersionV31 {
			return true
		}
	}
	return false
}

type TransmitterType int

const (
	TransmitterTypeCRE TransmitterType = iota
)

func (t TransmitterType) String() string {
	switch t {
	case TransmitterTypeCRE:
		return "cre"
	default:
		return fmt.Sprintf("unknown transmitter type: %d", t)
	}
}

func (t *TransmitterType) UnmarshalText(text []byte) error {
	switch string(text) {
	case "cre":
		*t = TransmitterTypeCRE
	default:
		return fmt.Errorf("unknown transmitter type: %s", text)
	}
	return nil
}

type TransmitterConfig struct {
	Type TransmitterType `json:"type" toml:"type"`
	// each sub-transmitter can have its own specific configuration
	Opts json.RawMessage `json:"opts" toml:"opts"`
}

// MaxPluginConfigSize is a sanity limit on the size of the JSON-encoded
// plugin config. It is generous enough to hold inline ChannelDefinitions for
// very large DONs, while preventing unbounded allocations from a malformed or
// hostile config blob.
const MaxPluginConfigSize = 8 * 1024 * 1024 // 8 MiB

func (p *PluginConfig) Unmarshal(data []byte) error {
	if len(data) > MaxPluginConfigSize {
		return fmt.Errorf("llo: plugin config too large; got %d bytes, max %d bytes", len(data), MaxPluginConfigSize)
	}
	return json.Unmarshal(data, p)
}

func (p PluginConfig) GetServers() (servers []mercuryconfig.Server) {
	for url, pubKey := range p.Servers {
		servers = append(servers, mercuryconfig.Server{URL: wssRegexp.ReplaceAllString(url, ""), PubKey: pubKey})
	}
	sort.Slice(servers, func(i, j int) bool {
		return servers[i].URL < servers[j].URL
	})
	return
}

func (p PluginConfig) Validate() (merr error) {
	if p.DonID == 0 {
		merr = errors.Join(merr, errors.New("llo: DonID must be specified and not zero"))
	}

	if len(p.Servers) == 0 && len(p.Transmitters) == 0 {
		merr = errors.Join(merr, errors.New("llo: At least one Mercury server or Transmitter must be specified"))
	} else {
		for serverName, serverPubKey := range p.Servers {
			if err := validateURL(serverName); err != nil {
				merr = errors.Join(merr, fmt.Errorf("llo: invalid value for ServerURL: %w", err))
			}
			if len(serverPubKey) != 32 {
				merr = errors.Join(merr, errors.New("llo: ServerPubKey must be a 32-byte hex string"))
			}
		}
	}

	if p.ChannelDefinitions != "" {
		if p.ChannelDefinitionsContractAddress != (common.Address{}) {
			merr = errors.Join(merr, errors.New("llo: ChannelDefinitionsContractAddress is not allowed if ChannelDefinitions is specified"))
		}
		if p.ChannelDefinitionsContractFromBlock != 0 {
			merr = errors.Join(merr, errors.New("llo: ChannelDefinitionsContractFromBlock is not allowed if ChannelDefinitions is specified"))
		}
		var cd llotypes.ChannelDefinitions
		if err := json.Unmarshal([]byte(p.ChannelDefinitions), &cd); err != nil {
			merr = errors.Join(merr, fmt.Errorf("channelDefinitions is invalid JSON: %w", err))
		}
	} else if p.ChannelDefinitionsContractAddress == (common.Address{}) {
		merr = errors.Join(merr, errors.New("llo: ChannelDefinitionsContractAddress is required if ChannelDefinitions is not specified"))
	}

	merr = errors.Join(merr, validateKeyBundleIDs(p.KeyBundleIDs))

	merr = errors.Join(merr, validatePluginVersion("PluginVersion", p.PluginVersion))

	if len(p.PluginVersions) > MaxProtocolInstances {
		merr = errors.Join(merr, fmt.Errorf("llo: PluginVersions must have at most %d entries, one per protocol instance, got: %d", MaxProtocolInstances, len(p.PluginVersions)))
	}
	for i, v := range p.PluginVersions {
		merr = errors.Join(merr, validatePluginVersion(fmt.Sprintf("PluginVersions[%d]", i), v))
	}

	if p.AnyV31() {
		merr = errors.Join(merr, p.V31.Validate())
	} else if !p.V31.IsZero() {
		merr = errors.Join(merr, fmt.Errorf("llo: V31 config is only allowed when a protocol instance runs %q", PluginVersionV31))
	}

	return merr
}

func validatePluginVersion(field, v string) error {
	switch v {
	case "", PluginVersionV30, PluginVersionV31:
		return nil
	default:
		return fmt.Errorf("llo: %s must be one of %q, %q or empty, got: %q", field, PluginVersionV30, PluginVersionV31, v)
	}
}

func validateURL(rawServerURL string) error {
	var normalizedURI string
	if schemeRegexp.MatchString(rawServerURL) {
		normalizedURI = rawServerURL
	} else {
		normalizedURI = "wss://" + rawServerURL
	}
	uri, err := url.ParseRequestURI(normalizedURI)
	if err != nil {
		return fmt.Errorf(`llo: invalid value for ServerURL, got: %q`, rawServerURL)
	}
	if uri.Scheme != "wss" {
		return fmt.Errorf(`llo: invalid scheme specified for MercuryServer, got: %q (scheme: %q) but expected a websocket url e.g. "192.0.2.2:4242" or "wss://192.0.2.2:4242"`, rawServerURL, uri.Scheme)
	}
	return nil
}

func validateKeyBundleIDs(keyBundleIDs map[string]string) error {
	for k, v := range keyBundleIDs {
		if k == "" {
			return errors.New("llo: KeyBundleIDs: key must not be empty")
		}
		if v == "" {
			return errors.New("llo: KeyBundleIDs: value must not be empty")
		}
		if _, err := llotypes.ReportFormatFromString(k); err != nil {
			return fmt.Errorf("llo: KeyBundleIDs: key must be a recognized report format, got: %s (err: %w)", k, err)
		}
		if !corekeys.IsSupportedChainType(corekeys.ChainType(k)) {
			return fmt.Errorf("llo: KeyBundleIDs: key must be a supported chain type, got: %s", k)
		}
	}
	return nil
}

var schemeRegexp = regexp.MustCompile(`^[a-zA-Z][a-zA-Z0-9+.-]*://`)
var wssRegexp = regexp.MustCompile(`^wss://`)
