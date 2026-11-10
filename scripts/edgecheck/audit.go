package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
)

type auditRecord struct {
	ID    string
	URI   string
	Rules []string
	Raw   string
}

type auditCounts struct {
	Records         int            `json:"records"`
	Matches         int            `json:"matches"`
	WithoutMessages int            `json:"without_messages"`
	Rules           map[string]int `json:"rules"`
}

// Compose logs mix startup/access lines with JSON audit records. Accumulate
// complete JSON objects, respecting quoted braces, and reject every malformed
// or truncated record. Never infer matches from nginx's textual error log.
func parseAudit(input string) ([]auditRecord, error) {
	var records []auditRecord
	var pending bytes.Buffer
	depth, quoted, escaped := 0, false, false
	scan := bufio.NewScanner(strings.NewReader(input))
	scan.Buffer(make([]byte, 4096), 16<<20)
	for scan.Scan() {
		line := strings.TrimSpace(scan.Text())
		if pending.Len() == 0 && !strings.HasPrefix(line, "{") {
			if strings.Contains(line, `"transaction"`) || strings.HasPrefix(line, `"`) || strings.HasPrefix(line, "}") {
				return nil, fmt.Errorf("invalid start of audit record")
			}
			continue
		}
		pending.WriteString(line)
		pending.WriteByte('\n')
		for _, c := range line {
			if quoted {
				if escaped {
					escaped = false
				} else if c == '\\' {
					escaped = true
				} else if c == '"' {
					quoted = false
				}
			} else {
				switch c {
				case '"':
					quoted = true
				case '{', '[':
					depth++
				case '}', ']':
					depth--
				}
			}
		}
		if depth < 0 {
			return nil, fmt.Errorf("unbalanced audit record")
		}
		if depth != 0 || quoted {
			continue
		}
		var envelope struct {
			Transaction *struct {
				ID      string `json:"unique_id"`
				Request struct {
					URI string `json:"uri"`
				} `json:"request"`
				Messages []struct {
					Details struct {
						RuleID json.RawMessage `json:"ruleId"`
					} `json:"details"`
				} `json:"messages"`
			} `json:"transaction"`
		}
		if json.Unmarshal(pending.Bytes(), &envelope) != nil || envelope.Transaction == nil || envelope.Transaction.ID == "" {
			return nil, fmt.Errorf("invalid audit transaction")
		}
		record := auditRecord{ID: envelope.Transaction.ID, URI: envelope.Transaction.Request.URI, Raw: pending.String()}
		for _, message := range envelope.Transaction.Messages {
			id := strings.Trim(string(message.Details.RuleID), `"`)
			if id == "" || strings.Trim(id, "0123456789") != "" {
				return nil, fmt.Errorf("audit message has no numeric details.ruleId")
			}
			record.Rules = append(record.Rules, id)
		}
		records = append(records, record)
		pending.Reset()
	}
	if scan.Err() != nil || pending.Len() != 0 {
		return nil, fmt.Errorf("truncated audit record")
	}
	return records, nil
}

func countAudit(records []auditRecord, baseline map[string]bool) auditCounts {
	c := auditCounts{Rules: map[string]int{}}
	for _, record := range records {
		if baseline[record.ID] {
			continue
		}
		c.Records++
		if len(record.Rules) == 0 {
			c.WithoutMessages++
		}
		for _, rule := range record.Rules {
			c.Matches++
			c.Rules[rule]++
		}
	}
	return c
}

func positiveAudit(records []auditRecord, baseline map[string]bool) bool {
	for _, record := range records {
		if !baseline[record.ID] && strings.Contains(record.URI, "edgecheck_attack_marker=1") && len(record.Rules) > 0 {
			return true
		}
	}
	return false
}
