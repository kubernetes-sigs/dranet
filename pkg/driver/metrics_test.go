/*
Copyright The Kubernetes Authors

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    https://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package driver

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
)

func TestNRIPluginReconnectAttemptsTotal(t *testing.T) {
	before := testutil.ToFloat64(nriPluginReconnectAttemptsTotal)

	nriPluginReconnectAttemptsTotal.Inc()
	nriPluginReconnectAttemptsTotal.Inc()

	if got := testutil.ToFloat64(nriPluginReconnectAttemptsTotal) - before; got != 2 {
		t.Errorf("expected nriPluginReconnectAttemptsTotal to increase by 2, got %v", got)
	}
}

func TestNetdbReconnectAttemptsTotal(t *testing.T) {
	before := testutil.ToFloat64(netdbReconnectAttemptsTotal)

	netdbReconnectAttemptsTotal.Inc()
	netdbReconnectAttemptsTotal.Inc()
	netdbReconnectAttemptsTotal.Inc()

	if got := testutil.ToFloat64(netdbReconnectAttemptsTotal) - before; got != 3 {
		t.Errorf("expected netdbReconnectAttemptsTotal to increase by 3, got %v", got)
	}
}
