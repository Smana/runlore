// SPDX-License-Identifier: Apache-2.0

// Package factory is RunLore's client of the agent factory (agent-to-agent design): it files each
// finding as a task through the factory's API, records the task per TriggerKey in an append-only
// ledger, and follows the work through the broker's signed webhooks into the incident's Slack
// thread. The factory decides every gate; RunLore is an ordinary API client of it.
package factory
