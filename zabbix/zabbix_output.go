package plugins

import (
	"bytes"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/mathpl/active_zabbix"

	"github.com/mozilla-services/heka/message"
	. "github.com/mozilla-services/heka/pipeline"
)

// Output plugin that sends messages via TCP using the Heka protocol.
type ZabbixOutput struct {
	conf            *ZabbixOutputConfig
	key_filter      keyFilterMap
	key_seen_window time.Duration
	key_seen        keySeenMap
	zabbix_client   active_zabbix.ZabbixActiveClient
	report_chan     chan chan reportMsg
}

type keyFilterMap struct {
	m map[string]active_zabbix.HostActiveKeys
	sync.RWMutex
}

type keySeenMap struct {
	m map[string]HostSeenKeys
	sync.RWMutex
}

type reportMsg struct {
	name   string
	values []string
}

type HostActiveKeys map[string]time.Duration
type HostSeenKeys map[string]time.Time

// ConfigStruct for ZabbixOutputstruct plugin.
type ZabbixOutputConfig struct {
	// Zabbix server address
	Address string `toml:"address"`
	// Maximum interval between each send
	TickerInterval uint `toml:"ticker_interval"`
	// Time between each update from the zabbix server for key filtering
	ZabbixChecksPollInterval uint `toml:"zabbix_checks_poll_interval"`
	// Maximum key count retained when zabbix doesn't respond
	MaxKeyCount uint `toml:"max_key_count"`
	// This many keys will trigger a send
	SendKeyCount uint `toml:"send_key_count"`
	// Encoder to use
	Encoder string `toml:"encoder"`
	// Read deadline in ms
	ReceiveTimeout uint `toml:"receive_timeout"`
	// Write deadline in ms
	SendTimeout uint `toml:"send_timeout"`
	// Override hostname
	OverrideHostname string `toml:"override_hostname"`
	// Clean up key seen beyond that time
	KeySeenWindow uint `toml:"key_seen_window"`
}

func (zo *ZabbixOutput) ConfigStruct() interface{} {
	return &ZabbixOutputConfig{
		Encoder:                  "ZabbixEncoder",
		TickerInterval:           uint(15),
		ZabbixChecksPollInterval: uint(300),
		ReceiveTimeout:           uint(3),
		SendTimeout:              uint(1),
		SendKeyCount:             uint(1000),
		MaxKeyCount:              uint(2000),
		KeySeenWindow:            uint(0),
	}
}

func (zo *ZabbixOutput) Init(config interface{}) (err error) {
	zo.conf = config.(*ZabbixOutputConfig)

	zo.zabbix_client, err = active_zabbix.NewZabbixActiveClient(zo.conf.Address, zo.conf.ReceiveTimeout, zo.conf.SendTimeout)
	zo.report_chan = make(chan chan reportMsg, 1)
	zo.key_filter = keyFilterMap{m: make(map[string]active_zabbix.HostActiveKeys)}

	zo.key_seen_window = time.Duration(zo.conf.KeySeenWindow) * time.Second
	zo.key_seen = keySeenMap{m: make(map[string]HostSeenKeys)}

	if zo.conf.OverrideHostname != "" {
		zo.key_filter.m[zo.conf.OverrideHostname] = nil
	} else {
		var host string
		host, err = os.Hostname()
		zo.key_filter.m[host] = nil
	}

	// A bit of config validation
	if zo.conf.MaxKeyCount < zo.conf.SendKeyCount || zo.conf.SendKeyCount < 1 {
		err = fmt.Errorf("Invalid combinason of send_key_count and max_key_count: %d must be <= %d", zo.conf.SendKeyCount, zo.conf.MaxKeyCount)
	}

	if zo.conf.ZabbixChecksPollInterval != 0 && zo.conf.ZabbixChecksPollInterval <= zo.conf.ReceiveTimeout/1000 {
		err = fmt.Errorf("Invalid combinason of zabbix_checks_poll_interval and receive_timeout: %d must > %d", zo.conf.SendKeyCount, zo.conf.MaxKeyCount)
	}

	return
}

func (zo *ZabbixOutput) SendRecords(records [][]byte) (data_left [][]byte, err error) {
	//FIXME: Proper json encoding
	msgHeader := []byte("{\"request\":\"agent data\",\"data\":[")
	msgHeaderLength := len(msgHeader)

	msgClose := []byte("]}")
	msgCloseLength := len(msgClose)

	data_left = records

	for len(data_left) > 0 {
		length := 0
		if len(data_left) >= int(zo.conf.SendKeyCount) {
			length = int(zo.conf.SendKeyCount)
		} else {
			length = len(data_left)
		}

		joinedRecords := bytes.Join(data_left[:length], []byte(","))
		msgArray := make([]byte, msgHeaderLength+length+msgCloseLength)

		msgSlice := msgArray[0:0]
		msgSlice = append(msgSlice, msgHeader...)
		msgSlice = append(msgSlice, joinedRecords...)
		msgSlice = append(msgSlice, msgClose...)

		if err = zo.zabbix_client.ZabbixSendAndForget(msgSlice); err != nil {
			return data_left, err
		}

		// Move down the slice
		data_left = data_left[length:]
	}

	return
}

func (zo *ZabbixOutput) Filter(pack *PipelinePack) (discard bool, err error) {
	var (
		val   interface{}
		key   string
		host  string
		ok    bool
		found bool
	)

	discard = true

	if val, found = pack.Message.GetFieldValue("key"); !found {
		err = fmt.Errorf("No Key in message")
		pack.Recycle()
		return
	}
	if key, ok = val.(string); !ok {
		err = fmt.Errorf("Unable to cast key to string")
		pack.Recycle()
		return
	}

	if val, found = pack.Message.GetFieldValue("host"); !found {
		err = fmt.Errorf("No Host in message")
		pack.Recycle()
		return
	}
	if host, ok = val.(string); !ok {
		err = fmt.Errorf("Unable to cast host to string")
		pack.Recycle()
		return
	}

	// Populate key seen if enabled
	if zo.conf.KeySeenWindow != 0 {
		zo.key_seen.Lock()
		if hs, found := zo.key_seen.m[host]; !found || hs == nil {
			zo.key_seen.m[host] = make(HostSeenKeys, 1)
		}
		zo.key_seen.m[host][key] = time.Now()
		zo.key_seen.Unlock()
	}

	// Check against active check filter
	zo.key_filter.RLock()
	hc, found_host := zo.key_filter.m[host]
	zo.key_filter.RUnlock()

	if found_host && hc != nil {
		if _, found_key := hc[key]; found_key {
			discard = false
		}
	} else {
		// We have no data on current host, we'll need to fetch it!
		// Discard by default
		zo.key_filter.Lock()
		zo.key_filter.m[host] = nil
		zo.key_filter.Unlock()
	}

	return
}

func (zo *ZabbixOutput) SendMetrics(or OutputRunner, data [][]byte) (new_slice [][]byte, err error) {
	new_slice = data
	if new_slice, err = zo.SendRecords(data); err != nil {
		// If we've hit the max key to send truncate the slice down starting with the oldest
		if len(new_slice) > int(zo.conf.MaxKeyCount) {
			copy(data, new_slice)
			remove_tail := zo.conf.MaxKeyCount - zo.conf.SendKeyCount
			or.LogError(fmt.Errorf("Truncated %d oldest metrics from in-memory buffer.", zo.conf.SendKeyCount))
			new_slice = data[:remove_tail]
		}
		return
	}

	return
}

func (zo *ZabbixOutput) fetchZabbixChecks(or OutputRunner) {
	or.LogMessage("Updating key_filter with FetchActiveChecks")
	zo.key_filter.Lock()
	for host, _ := range zo.key_filter.m {
		if hc, localErr := zo.zabbix_client.FetchActiveChecks(host); localErr != nil {
			// Keep previous list if the server can't refresh the list of checks
			or.LogError(fmt.Errorf("Zabbix server unable to provide active check list for host %s: %s", host, localErr))
		} else {
			zo.key_filter.m[host] = hc
		}
	}
	zo.key_filter.Unlock()
}

func (zo *ZabbixOutput) Run(or OutputRunner, h PluginHelper) (err error) {
	var (
		ok       = true
		pack     *PipelinePack
		inChan   = or.InChan()
		ticker   = or.Ticker()
		stopChan = make(chan struct{})
	)

	// Goroutine to ask Zabbix for which checks this host should have
	if zo.conf.ZabbixChecksPollInterval > 0 {
		// Run on startup without waiting for a whole Tick interval
		zo.fetchZabbixChecks(or)
		go func() {
			for {
				select {
				case <-stopChan:
					or.LogMessage("Exiting ZabbixChecksPollInterval goroutine")
					return

				case <-time.Tick(time.Duration(zo.conf.ZabbixChecksPollInterval) * time.Second):
					zo.fetchZabbixChecks(or)
				}
			}
		}()
	}

	// Goroutine to clear out keys that haven't updated in key_seen_window
	if zo.conf.KeySeenWindow > 0 {
		go func() {
			for {
				select {
				case <-stopChan:
					or.LogMessage("Exiting KeySeenWindow goroutine")
					return

				case <-time.Tick(time.Duration(zo.conf.KeySeenWindow) * time.Second):
					or.LogMessage("Cleaning up keys")
					zo.key_seen.Lock()
					for host, hs := range zo.key_seen.m {
						for key, t := range hs {
							if time.Now().After(t.Add(zo.key_seen_window)) {
								delete(hs, key)
							}
						}
						if len(hs) == 0 {
							delete(zo.key_seen.m, host)
						}
					}
					zo.key_seen.Unlock()
				}
			}
		}()
	}

	dataArray := make([][]byte, zo.conf.MaxKeyCount)
	dataSlice := dataArray[0:0]
	for ok {
		select {

		case pack, ok = <-inChan:
			if !ok {
				close(stopChan)
				break
			}

			// Skip discard check if key filtering is disabled
			if zo.conf.ZabbixChecksPollInterval > 0 {
				discard, err := zo.Filter(pack)
				if err != nil {
					or.LogError(err)
				}
				if discard {
					pack.Recycle()
					continue
				}
			}

			if msg, localErr := or.Encode(pack); localErr != nil {
				or.LogError(fmt.Errorf("Encoder failure: %s", localErr))
				pack.Recycle()
				continue
			} else {
				dataSlice = append(dataSlice, msg)
			}
			pack.Recycle()

			if len(dataSlice) >= int(zo.conf.SendKeyCount) {
				if dataSlice, err = zo.SendMetrics(or, dataSlice); err != nil {
					or.LogError(err)
				}
			}

		case <-ticker:
			if !ok {
				break
			}

			if len(dataSlice) > 0 {
				if dataSlice, err = zo.SendMetrics(or, dataSlice); err != nil {
					or.LogError(err)
				}
			}

		case rchan := <-zo.report_chan:
			if !ok {
				break
			}
			zo.key_filter.RLock()
			for host, hc := range zo.key_filter.m {
				// Fix for js cutting at dot in the field name
				host = strings.Replace(host, ".", "_", -1)
				rm := reportMsg{name: fmt.Sprintf("ActiveChecks-%s", host)}
				if hc != nil {
					rm.values = make([]string, len(hc))
					vs := rm.values[0:0]
					for key, _ := range hc {
						vs = append(vs, key)
					}
					rchan <- rm
				}
			}
			zo.key_filter.RUnlock()

			zo.key_seen.RLock()
			for host, hs := range zo.key_seen.m {
				host = strings.Replace(host, ".", "_", -1)
				rm := reportMsg{name: fmt.Sprintf("KeySeen-%s", host)}
				if hs != nil {
					rm.values = make([]string, len(hs))
					vs := rm.values[0:0]
					for key, _ := range hs {
						vs = append(vs, key)
					}
					rchan <- rm
				}
			}
			zo.key_seen.RUnlock()

			close(rchan)
		}
	}

	return
}

func init() {
	RegisterPlugin("ZabbixOutput", func() interface{} {
		return new(ZabbixOutput)
	})
}

// ReportMsg provides plugin state to Heka report and dashboard.
func (zo *ZabbixOutput) ReportMsg(msg *message.Message) error {
	rchan := make(chan reportMsg, 1)
	zo.report_chan <- rchan

	for rm := range rchan {
		sort.Strings(rm.values)
		joined_values := strings.Join(rm.values, " ")
		message.NewStringField(msg, rm.name, joined_values)
	}

	return nil
}
