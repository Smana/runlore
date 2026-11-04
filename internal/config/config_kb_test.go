// SPDX-License-Identifier: Apache-2.0

package config

import (
	"strings"
	"testing"
)

func TestServerMCPNeedsCatalog(t *testing.T) {
	if _, err := loadString(t, "server: {mcp: {enabled: true}}\n"); err == nil || !strings.Contains(err.Error(), "server.mcp.enabled needs catalog.dir") {
		t.Fatalf("got %v", err)
	}
	c, err := loadString(t, "catalog: {dir: /var/lib/runlore/catalog}\nserver: {mcp: {enabled: true, token_env: KB_TOKEN}}\n")
	if err != nil || !c.Server.MCP.Enabled || c.Server.MCP.TokenEnv != "KB_TOKEN" {
		t.Fatalf("%v %+v", err, c.Server.MCP)
	}
}
