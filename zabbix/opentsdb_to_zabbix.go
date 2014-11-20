/***** BEGIN LICENSE BLOCK *****
# This Source Code Form is subject to the terms of the Mozilla Public
# License, v. 2.0. If a copy of the MPL was not distributed with this file,
# You can obtain one at http://mozilla.org/MPL/2.0/.
#
# The Initial Developer of the Original Code is the Mozilla Foundation.
# Portions created by the Initial Developer are Copyright (C) 2014
# the Initial Developer. All Rights Reserved.
#
# Contributor(s):
#   Mathieu Payeur Levallois (math.pay@gmail.com)
#
# ***** END LICENSE BLOCK *****/

package plugins

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/mozilla-services/heka/pipeline"
)

type OpenTsdbToZabbixEncoder struct {
	config *OpenTsdbToZabbixEncoderConfig
}

type OpenTsdbToZabbixEncoderConfig struct {
}

type zabbixMetricJson struct {
	Host  string
	Key   string
	Value string
	Clock string
}

func (oze *OpenTsdbToZabbixEncoder) ConfigStruct() interface{} {
	return &OpenTsdbToZabbixEncoderConfig{}
}

func (oze *OpenTsdbToZabbixEncoder) Init(config interface{}) (err error) {
	oze.config = config.(*OpenTsdbToZabbixEncoderConfig)

	return
}

func fieldToString(fieldName string, pack *pipeline.PipelinePack) (val string, err error) {
	var (
		tmp interface{}
		ok  bool
	)

	if tmp, ok = pack.Message.GetFieldValue(fieldName); !ok {
		err = fmt.Errorf("Unable to find fieldname: %s", fieldName)
		return
	}

	if val, ok = tmp.(string); !ok {
		err = fmt.Errorf("Unable to cast field to string: %s", fieldName)
		return
	}

	return
}

func (oe *OpenTsdbToZabbixEncoder) Encode(pack *pipeline.PipelinePack) (output []byte, err error) {
	var zm zabbixMetricJson

	zm.Clock = fmt.Sprintf("%d", time.Unix(0, pack.Message.GetTimestamp()).UTC().Unix())

	if zm.Key, err = fieldToString("Key", pack); err != nil {
		return nil, err
	}
	if zm.Host, err = fieldToString("Host", pack); err != nil {
		return nil, err
	}
	if zm.Value, err = fieldToString("Value", pack); err != nil {
		return nil, err
	}

	output, err = json.Marshal(zm)

	return
}

func init() {
	pipeline.RegisterPlugin("OpenTsdbToZabbixEncoder", func() interface{} {
		return new(OpenTsdbToZabbixEncoder)
	})
}
