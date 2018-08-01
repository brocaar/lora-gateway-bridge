package gateway

import (
	"encoding/json"
	"fmt"
	"io/ioutil"
	"os/exec"
	"regexp"
	"sort"
	"strings"

	"github.com/pkg/errors"

	"github.com/brocaar/loraserver/api/gw"
	"github.com/brocaar/lorawan"
	"github.com/brocaar/lorawan/band"
)

// radioBandwidthPerChannelBandwidth defines the bandwidth that a single radio
// can cover per channel bandwidth
var radioBandwidthPerChannelBandwidth = map[int]int{
	500000: 1100000, // 500kHz channel
	250000: 1000000, // 250kHz channel
	125000: 925000,  // 125kHz channel
}

// defaultRadioBandwidth defines the radio bandwidth in case the channel
// bandwidth does not match any of the above values.
const defaultRadioBandwidth = 925000

// jsonCommentRegexp matches json comments
var jsonCommentRegexp = regexp.MustCompile(`/\*.*\*/`)

// radioConfig contains the radio configuration.
type radioConfig struct {
	Enable bool
	Freq   int
}

// multiSFChannelConfig contains the configuration for a multi spreading-factor
// channel.
type multiSFChannelConfig struct {
	Enable bool
	Radio  int
	IF     int
	Freq   int
}

// LoRaSTDChannelConfig contains the configuration for a single
// spreading-factor LoRa channel.
type loRaSTDChannelConfig struct {
	Enable       bool
	Radio        int
	IF           int
	Bandwidth    int
	SpreadFactor int
	Freq         int
}

// fskChannelConfig contains the configuratio for a FSK channel.
type fskChannelConfig struct {
	Enable    bool
	Radio     int
	IF        int
	Bandwidth int
	DataRate  int
	Freq      int
}

// configFile represents a packet-forwarder JSON config file.
type configFile struct {
	SX1301Conf  map[string]interface{} `json:"SX1301_conf"`
	GatewayConf map[string]interface{} `json:"gateway_conf"`
}

// gatewayConfiguration contains the radio configuration for a gateway.
type gatewayConfiguration struct {
	Radios               []radioConfig
	MultiSFChannels      []multiSFChannelConfig
	LoRaSTDChannelConfig loRaSTDChannelConfig
	FSKChannelConfig     fskChannelConfig
}

// channelByMinRadioCenterFreqency implements sort.Interface for []*gw.Channel.
// The sorting is based on the center frequency of the radio when placing the
// channel exactly on the left side of the available radio bandwidth.
type channelByMinRadioCenterFrequency []gw.Channel

func (c channelByMinRadioCenterFrequency) Len() int      { return len(c) }
func (c channelByMinRadioCenterFrequency) Swap(i, j int) { c[i], c[j] = c[j], c[i] }
func (c channelByMinRadioCenterFrequency) Less(i, j int) bool {
	return c.minRadioCenterFreq(i) < c.minRadioCenterFreq(j)
}
func (c channelByMinRadioCenterFrequency) minRadioCenterFreq(i int) int {
	channelBandwidth := int(c[i].Bandwidth * 1000)
	radioBandwidth, ok := radioBandwidthPerChannelBandwidth[channelBandwidth]
	if !ok {
		radioBandwidth = defaultRadioBandwidth
	}
	return int(c[i].Frequency) - (channelBandwidth / 2) + (radioBandwidth / 2)
}

// getGatewayConfig transforms the given GatewayConfigurationPacket into a
// gatewayConfiguration object. It will determine the configuration for the
// radios and their center frequencies and the channels assigned to each radio.
func getGatewayConfig(conf gw.GatewayConfigPacket) (gatewayConfiguration, error) {
	var gc gatewayConfiguration

	// make sure the channels are sorted by the minimum radio center frequency
	sort.Sort(channelByMinRadioCenterFrequency(conf.Channels))

	// define the radios and their center frequency
	for _, c := range conf.Channels {
		channelBandwidth := c.Bandwidth * 1000
		channelMin := c.Frequency - (channelBandwidth / 2)
		channelMax := c.Frequency + (channelBandwidth / 2)
		radioBandwidth, ok := radioBandwidthPerChannelBandwidth[channelBandwidth]
		if !ok {
			radioBandwidth = defaultRadioBandwidth
		}

		var found bool
		var radio int

		for i, r := range gc.Radios {
			if channelMin >= r.Freq-(radioBandwidth/2) && channelMax <= r.Freq+(radioBandwidth/2) {
				found = true
				radio = i
				break
			}
		}

		if !found {
			// No sutiable radio found, add a new one
			newRadio := radioConfig{
				Enable: true,
				Freq:   c.Frequency - (channelBandwidth / 2) + (radioBandwidth / 2),
			}
			gc.Radios = append(gc.Radios, newRadio)
			radio = len(gc.Radios) - 1
		}

		if c.Modulation == band.FSKModulation {
			// FSK channel
			if gc.FSKChannelConfig.Enable {
				return gc, errors.New("gateway: fsk channel already configured")
			}

			gc.FSKChannelConfig = fskChannelConfig{
				Enable:    true,
				Radio:     radio,
				IF:        c.Frequency - gc.Radios[radio].Freq,
				Bandwidth: c.Bandwidth,
				DataRate:  c.Bitrate,
				Freq:      c.Frequency,
			}

		} else if c.Modulation == band.LoRaModulation && len(c.SpreadingFactors) == 1 {
			// LoRa STD (single SF) channel
			if gc.LoRaSTDChannelConfig.Enable {
				return gc, errors.New("gateway: lora std channel already configured")
			}

			gc.LoRaSTDChannelConfig = loRaSTDChannelConfig{
				Enable:       true,
				Radio:        radio,
				IF:           c.Frequency - gc.Radios[radio].Freq,
				Bandwidth:    channelBandwidth,
				SpreadFactor: c.SpreadingFactors[0],
				Freq:         c.Frequency,
			}

		} else if c.Modulation == band.LoRaModulation {
			// LoRa multi-SF channels
			multiSFChannel := multiSFChannelConfig{
				Enable: true,
				Radio:  radio,
				IF:     c.Frequency - gc.Radios[radio].Freq,
				Freq:   c.Frequency,
			}
			gc.MultiSFChannels = append(gc.MultiSFChannels, multiSFChannel)

		} else {
			return gc, fmt.Errorf("gateway: invalid modulation: %s", c.Modulation)
		}
	}

	return gc, nil
}

func loadConfigFile(filePath string) (configFile, error) {
	var out configFile
	b, err := ioutil.ReadFile(filePath)
	if err != nil {
		return out, errors.Wrap(err, "read file error")
	}

	// remove comments from json
	b = jsonCommentRegexp.ReplaceAll(b, []byte{})

	if err = json.Unmarshal(b, &out); err != nil {
		return out, errors.Wrap(err, "unmarshal config json error")
	}

	return out, nil
}

// mergeConfig merges the new configuration into the given configuration.
// Unfortunately we have to do this as the packet-forwarder sees these keys
// as complete overrides (it does not just update the leaves).
// We want to remain the other configuration (e.g. which radio chip is used,
// calibration values that are board specific).
// This is not pretty but it works.
func mergeConfig(mac lorawan.EUI64, config configFile, newConfig gatewayConfiguration) error {
	// update radios
	for i, r := range newConfig.Radios {
		key := fmt.Sprintf("radio_%d", i)
		radio, ok := config.SX1301Conf[key].(map[string]interface{})
		if !ok {
			return fmt.Errorf("expected %s to be of type map[string]interface{}, got %T", key, config.SX1301Conf[key])
		}
		radio["enable"] = r.Enable
		radio["freq"] = r.Freq
	}

	// update multi SF channels
	for i, c := range newConfig.MultiSFChannels {
		key := fmt.Sprintf("chan_multiSF_%d", i)
		channel, ok := config.SX1301Conf[key].(map[string]interface{})
		if !ok {
			return fmt.Errorf("expected %s to be of type map[string]interface{}, got %T", key, config.SX1301Conf[key])
		}
		channel["enable"] = c.Enable
		channel["radio"] = c.Radio
		channel["if"] = c.IF
	}

	// update LoRa std channel
	channel, ok := config.SX1301Conf["chan_Lora_std"].(map[string]interface{})
	if !ok {
		return fmt.Errorf("expected chan_Lora_std to be of type map[string]interface{}, got %T", config.SX1301Conf["chan_Lora_std"])
	}
	channel["enable"] = newConfig.LoRaSTDChannelConfig.Enable
	channel["radio"] = newConfig.LoRaSTDChannelConfig.Radio
	channel["if"] = newConfig.LoRaSTDChannelConfig.IF
	channel["bandwidth"] = newConfig.LoRaSTDChannelConfig.Bandwidth
	channel["spread_factor"] = newConfig.LoRaSTDChannelConfig.SpreadFactor

	// update FSK channel
	channel, ok = config.SX1301Conf["chan_FSK"].(map[string]interface{})
	if !ok {
		return fmt.Errorf("expected chan_FSK to be of type map[string]interface{}, got %T", config.SX1301Conf["chan_FSK"])
	}
	channel["enable"] = newConfig.FSKChannelConfig.Enable
	channel["radio"] = newConfig.FSKChannelConfig.Radio
	channel["if"] = newConfig.FSKChannelConfig.IF
	channel["bandwidth"] = newConfig.FSKChannelConfig.Bandwidth
	channel["datarate"] = newConfig.FSKChannelConfig.DataRate

	// update gateway mac / ID
	config.GatewayConf["gateway_ID"] = mac.String()

	return nil
}

func invokePFRestart(cmd string) error {
	parts := strings.Fields(cmd)
	if len(parts) == 0 {
		return errors.New("gateway: no packet-forwarder restart command configured")
	}

	var args []string
	if len(parts) > 1 {
		args = parts[1:len(parts)]
	}

	_, err := exec.Command(parts[0], args...).Output()
	if err != nil {
		return errors.Wrap(err, "execute command error")
	}

	return nil
}
