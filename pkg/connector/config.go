package connector

import (
	_ "embed"
	"fmt"
	"net"
	"strings"
	"text/template"
	"time"

	up "go.mau.fi/util/configupgrade"
	"go.mau.fi/whatsmeow/types"
	"gopkg.in/yaml.v3"
	"maunium.net/go/mautrix/event"

	"go.mau.fi/mautrix-whatsapp/pkg/msgconv"
)

type MediaRequestMethod string

const (
	MediaRequestMethodImmediate MediaRequestMethod = "immediate"
	MediaRequestMethodLocalTime MediaRequestMethod = "local_time"
)

//go:embed example-config.yaml
var ExampleConfig string

type Config struct {
	OSName      string `yaml:"os_name"`
	BrowserName string `yaml:"browser_name"`

	Proxy          string `yaml:"proxy"`
	GetProxyURL    string `yaml:"get_proxy_url"`
	ProxyOnlyLogin bool   `yaml:"proxy_only_login"`

	DisplaynameTemplate string `yaml:"displayname_template"`

	CallStartNotices            bool             `yaml:"call_start_notices"`
	VoiceCalls                  VoiceCallsConfig `yaml:"voice_calls"`
	IdentityChangeNotices       bool             `yaml:"identity_change_notices"`
	SendPresenceOnTyping        bool             `yaml:"send_presence_on_typing"`
	EnableStatusBroadcast       bool             `yaml:"enable_status_broadcast"`
	DisableStatusBroadcastSend  bool             `yaml:"disable_status_broadcast_send"`
	MuteStatusBroadcast         bool             `yaml:"mute_status_broadcast"`
	StatusBroadcastTag          event.RoomTag    `yaml:"status_broadcast_tag"`
	PinnedTag                   event.RoomTag    `yaml:"pinned_tag"`
	ArchiveTag                  event.RoomTag    `yaml:"archive_tag"`
	WhatsappThumbnail           bool             `yaml:"whatsapp_thumbnail"`
	URLPreviews                 bool             `yaml:"url_previews"`
	ExtEvPolls                  bool             `yaml:"extev_polls"`
	DisableViewOnce             bool             `yaml:"disable_view_once"`
	ForceActiveDeliveryReceipts bool             `yaml:"force_active_delivery_receipts"`
	DirectMediaAutoRequest      bool             `yaml:"direct_media_auto_request"`
	InitialAutoReconnect        bool             `yaml:"initial_auto_reconnect"`
	UseWhatsAppRetryStore       bool             `yaml:"use_whatsapp_retry_store"`

	AnimatedSticker msgconv.AnimatedStickerConfig `yaml:"animated_sticker"`

	HistorySync struct {
		MaxInitialConversations int           `yaml:"max_initial_conversations"`
		RequestFullSync         bool          `yaml:"request_full_sync"`
		DispatchWait            time.Duration `yaml:"dispatch_wait"`
		FullSyncConfig          struct {
			DaysLimit    uint32 `yaml:"days_limit"`
			SizeLimit    uint32 `yaml:"size_mb_limit"`
			StorageQuota uint32 `yaml:"storage_quota_mb"`
		} `yaml:"full_sync_config"`

		MediaRequests struct {
			AutoRequestMedia bool               `yaml:"auto_request_media"`
			RequestMethod    MediaRequestMethod `yaml:"request_method"`
			RequestLocalTime int                `yaml:"request_local_time"`
			MaxAsyncHandle   int64              `yaml:"max_async_handle"`
		} `yaml:"media_requests"`

		BackwardsOnDemand bool `yaml:"backwards_on_demand"`
	} `yaml:"history_sync"`

	displaynameTemplate *template.Template `yaml:"-"`
}

type VoiceCallsConfig struct {
	Enabled               bool          `yaml:"enabled"`
	Incoming              bool          `yaml:"incoming"`
	Outgoing              bool          `yaml:"outgoing"`
	Video                 bool          `yaml:"video"`
	MaxConcurrentPerLogin int           `yaml:"max_concurrent_per_login"`
	RingTimeout           time.Duration `yaml:"ring_timeout"`
	ConnectTimeout        time.Duration `yaml:"connect_timeout"`
	UDPPortMin            uint16        `yaml:"udp_port_min"`
	UDPPortMax            uint16        `yaml:"udp_port_max"`
	PublicIP              string        `yaml:"public_ip"`
	OpusBitrate           int           `yaml:"opus_bitrate"`
	TURNURIs              []string      `yaml:"turn_uris"`
	TURNSharedSecret      string        `yaml:"turn_shared_secret"`
	TURNTTL               time.Duration `yaml:"turn_ttl"`
}

type umConfig Config

func (c *Config) UnmarshalYAML(node *yaml.Node) error {
	err := node.Decode((*umConfig)(c))
	if err != nil {
		return err
	}
	return c.PostProcess()
}

func (c *Config) PostProcess() error {
	if c.VoiceCalls.Enabled {
		if c.VoiceCalls.MaxConcurrentPerLogin < 1 {
			return fmt.Errorf("voice_calls.max_concurrent_per_login must be at least 1")
		}
		if c.VoiceCalls.RingTimeout <= 0 {
			return fmt.Errorf("voice_calls.ring_timeout must be positive")
		}
		if c.VoiceCalls.ConnectTimeout <= 0 {
			return fmt.Errorf("voice_calls.connect_timeout must be positive")
		}
		if c.VoiceCalls.UDPPortMin == 0 || c.VoiceCalls.UDPPortMax < c.VoiceCalls.UDPPortMin {
			return fmt.Errorf("voice_calls UDP port range is invalid")
		}
		if c.VoiceCalls.PublicIP != "" && net.ParseIP(c.VoiceCalls.PublicIP) == nil {
			return fmt.Errorf("voice_calls.public_ip must be an IPv4 or IPv6 address")
		}
		if c.VoiceCalls.OpusBitrate < 6000 || c.VoiceCalls.OpusBitrate > 510000 {
			return fmt.Errorf("voice_calls.opus_bitrate must be between 6000 and 510000")
		}
		if len(c.VoiceCalls.TURNURIs) > 0 && c.VoiceCalls.TURNSharedSecret == "" {
			return fmt.Errorf("voice_calls.turn_shared_secret is required when turn_uris is configured")
		}
		if c.VoiceCalls.TURNTTL <= 0 {
			return fmt.Errorf("voice_calls.turn_ttl must be positive")
		}
	}
	var err error
	c.displaynameTemplate, err = template.New("displayname").Parse(c.DisplaynameTemplate)
	if err != nil {
		return err
	}
	// Try to execute template to make sure it's valid
	_, err = c.formatDisplayname(types.PSAJID, "", types.ContactInfo{})
	if err != nil {
		return fmt.Errorf("failed to execute displayname template: %w", err)
	}
	return nil
}

func upgradeConfig(helper up.Helper) {
	helper.Copy(up.Str, "os_name")
	helper.Copy(up.Str, "browser_name")

	helper.Copy(up.Str|up.Null, "proxy")
	helper.Copy(up.Str|up.Null, "get_proxy_url")
	helper.Copy(up.Bool, "proxy_only_login")

	helper.Copy(up.Str, "displayname_template")

	helper.Copy(up.Bool, "call_start_notices")
	helper.Copy(up.Bool, "voice_calls", "enabled")
	helper.Copy(up.Bool, "voice_calls", "incoming")
	helper.Copy(up.Bool, "voice_calls", "outgoing")
	helper.Copy(up.Bool, "voice_calls", "video")
	helper.Copy(up.Int, "voice_calls", "max_concurrent_per_login")
	helper.Copy(up.Str|up.Int, "voice_calls", "ring_timeout")
	helper.Copy(up.Str|up.Int, "voice_calls", "connect_timeout")
	helper.Copy(up.Int, "voice_calls", "udp_port_min")
	helper.Copy(up.Int, "voice_calls", "udp_port_max")
	helper.Copy(up.Str|up.Null, "voice_calls", "public_ip")
	helper.Copy(up.Int, "voice_calls", "opus_bitrate")
	helper.Copy(up.List, "voice_calls", "turn_uris")
	helper.Copy(up.Str|up.Null, "voice_calls", "turn_shared_secret")
	helper.Copy(up.Str|up.Int, "voice_calls", "turn_ttl")
	helper.Copy(up.Bool, "identity_change_notices")
	helper.Copy(up.Bool, "send_presence_on_typing")
	helper.Copy(up.Bool, "enable_status_broadcast")
	helper.Copy(up.Bool, "disable_status_broadcast_send")
	helper.Copy(up.Bool, "mute_status_broadcast")
	helper.Copy(up.Str|up.Null, "status_broadcast_tag")
	helper.Copy(up.Str|up.Null, "pinned_tag")
	helper.Copy(up.Str|up.Null, "archive_tag")
	helper.Copy(up.Bool, "whatsapp_thumbnail")
	helper.Copy(up.Bool, "url_previews")
	helper.Copy(up.Bool, "extev_polls")
	helper.Copy(up.Bool, "disable_view_once")
	helper.Copy(up.Bool, "force_active_delivery_receipts")
	helper.Copy(up.Bool, "direct_media_auto_request")
	helper.Copy(up.Bool, "initial_auto_reconnect")
	helper.Copy(up.Bool, "use_whatsapp_retry_store")

	helper.Copy(up.Str, "animated_sticker", "target")
	helper.Copy(up.Int, "animated_sticker", "args", "width")
	helper.Copy(up.Int, "animated_sticker", "args", "height")
	helper.Copy(up.Int, "animated_sticker", "args", "fps")

	helper.Copy(up.Int, "history_sync", "max_initial_conversations")
	helper.Copy(up.Bool, "history_sync", "request_full_sync")
	helper.Copy(up.Str|up.Int, "history_sync", "dispatch_wait")
	helper.Copy(up.Int|up.Null, "history_sync", "full_sync_config", "days_limit")
	helper.Copy(up.Int|up.Null, "history_sync", "full_sync_config", "size_mb_limit")
	helper.Copy(up.Int|up.Null, "history_sync", "full_sync_config", "storage_quota_mb")
	helper.Copy(up.Bool, "history_sync", "media_requests", "auto_request_media")
	helper.Copy(up.Str, "history_sync", "media_requests", "request_method")
	helper.Copy(up.Int, "history_sync", "media_requests", "request_local_time")
	helper.Copy(up.Int, "history_sync", "media_requests", "max_async_handle")
	helper.Copy(up.Bool, "history_sync", "backwards_on_demand")
}

type DisplaynameParams struct {
	types.ContactInfo
	Phone string

	// Deprecated legacy fields
	JID    string
	Notify string
	VName  string
	Name   string
	Short  string
}

func (c *Config) formatDisplayname(jid types.JID, phone string, contact types.ContactInfo) (string, error) {
	var nameBuf strings.Builder
	if phone == "" && jid.Server == types.DefaultUserServer {
		phone = "+" + jid.User
	}
	if contact.RedactedPhone == "" && phone != "" {
		contact.RedactedPhone = redactPhone(phone)
	}
	err := c.displaynameTemplate.Execute(&nameBuf, &DisplaynameParams{
		ContactInfo: contact,
		Phone:       phone,

		// Deprecated legacy fields
		JID:    phone,
		Notify: contact.PushName,
		VName:  contact.BusinessName,
		Name:   contact.FullName,
		Short:  contact.FirstName,
	})
	return nameBuf.String(), err
}

func (c *Config) FormatDisplayname(jid types.JID, phone string, contact types.ContactInfo) string {
	name, err := c.formatDisplayname(jid, phone, contact)
	if err != nil {
		panic(err)
	}
	return name
}

func redactPhone(phone string) string {
	if len(phone) <= 4 {
		return phone
	}
	// This doesn't keep 2+ digit country codes properly, but whatever
	return phone[:2] + strings.Repeat("∙", len(phone)-4) + phone[len(phone)-2:]
}

func (wa *WhatsAppConnector) GetConfig() (string, any, up.Upgrader) {
	return ExampleConfig, &wa.Config, &up.StructUpgrader{
		SimpleUpgrader: up.SimpleUpgrader(upgradeConfig),
		Blocks: [][]string{
			{"proxy"},
			{"displayname_template"},
			{"call_start_notices"},
			{"history_sync"},
		},
		Base: ExampleConfig,
	}
}
