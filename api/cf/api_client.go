package cf

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const baseURL = "https://api.cloudflare.com/client/v4"

// CFClient 是 Cloudflare API 的最小客户端，仅支持 Zone 查找和 DNS 记录创建。
type CFClient struct {
	Token string
	HTTP  *http.Client
}

// NewClient 创建 CFClient。
func NewClient(token string) *CFClient {
	return &CFClient{
		Token: token,
		HTTP:  &http.Client{Timeout: 15 * time.Second},
	}
}

// Zone represents a Cloudflare zone (partial).
type Zone struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// DNSRecord represents a Cloudflare DNS record (partial).
type DNSRecord struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Name     string `json:"name"`
	Content  string `json:"content"`
	Proxied  bool   `json:"proxied"`
	TTL      int    `json:"ttl"`
	Priority int    `json:"priority"`
}

// cfResponse is a generic Cloudflare API response wrapper.
type cfResponse struct {
	Success  bool        `json:"success"`
	Errors   []cfError   `json:"errors"`
	Messages []string    `json:"messages"`
	Result   interface{} `json:"result"`
}

type cfError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// FindZone 根据域名查找对应的 Cloudflare Zone。
// 例如输入 "vet.nightunderfly.online" 会查找 "nightunderfly.online" 对应的 Zone。
// 注意：此方法通过去掉域名第一段来猜测 zone，对多级子域名不准确。
// 推荐使用 FindZoneByName 直接指定 zone 名称。
func (c *CFClient) FindZone(domain string) (*Zone, error) {
	parts := strings.Split(domain, ".")
	if len(parts) < 2 {
		return nil, fmt.Errorf("invalid domain: %s", domain)
	}
	zoneName := strings.Join(parts[1:], ".")
	return c.FindZoneByName(zoneName)
}

// FindZoneByName 直接按名称查找 Cloudflare Zone。
func (c *CFClient) FindZoneByName(zoneName string) (*Zone, error) {
	req, _ := http.NewRequest("GET", baseURL+"/zones?name="+zoneName, nil)
	req.Header.Set("Authorization", "Bearer "+c.Token)

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("CF API request failed: %w", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	var result cfResponse
	result.Result = &[]Zone{}
	if err := json.Unmarshal(body, &result); err != nil {
		return nil, fmt.Errorf("CF API parse error: %w", err)
	}

	if !result.Success {
		msg := "unknown error"
		if len(result.Errors) > 0 {
			msg = result.Errors[0].Message
		}
		return nil, fmt.Errorf("CF API error: %s", msg)
	}

	zones := result.Result.(*[]Zone)
	if len(*zones) == 0 {
		return nil, fmt.Errorf("zone not found: %s", zoneName)
	}

	return &(*zones)[0], nil
}

// CreateMXRecord 在指定 Zone 下创建 MX 记录。
// subdomain 是相对 Zone 的子域名部分，如 "vet"。
// target 是 MX 记录指向的邮件服务器，如 "mail.nightunderfly.online"。
func (c *CFClient) CreateMXRecord(zoneID, subdomain, target string) (*DNSRecord, error) {
	record := DNSRecord{
		Type:     "MX",
		Name:     subdomain,
		Content:  target,
		Priority: 10,
		Proxied:  false, // MX 记录不能代理
		TTL:      1,     // Auto
	}

	payload, _ := json.Marshal(record)
	req, _ := http.NewRequest("POST", baseURL+"/zones/"+zoneID+"/dns_records", strings.NewReader(string(payload)))
	req.Header.Set("Authorization", "Bearer "+c.Token)
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("CF API request failed: %w", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	var result cfResponse
	result.Result = &DNSRecord{}
	if err := json.Unmarshal(body, &result); err != nil {
		return nil, fmt.Errorf("CF API parse error: %w", err)
	}

	if !result.Success {
		msg := "unknown error"
		if len(result.Errors) > 0 {
			msg = result.Errors[0].Message
		}
		return nil, fmt.Errorf("CF API error: %s", msg)
	}

	created := result.Result.(*DNSRecord)
	return created, nil
}

// FindMXRecord searches for MX records under a zone that match the given subdomain.
// zoneName is the full zone name (e.g. "nightunderfly.online"), subdomain is the relative
// part (e.g. "nut"). CF API requires the full FQDN in the name parameter.
// Returns the matching DNSRecord or nil if not found.
func (c *CFClient) FindMXRecord(zoneID, subdomain, zoneName, target string) (*DNSRecord, error) {
	fqdn := subdomain + "." + zoneName
	req, _ := http.NewRequest("GET", baseURL+"/zones/"+zoneID+"/dns_records?type=MX&name="+url.QueryEscape(fqdn), nil)
	req.Header.Set("Authorization", "Bearer "+c.Token)

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("CF API request failed: %w", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	var result cfResponse
	result.Result = &[]DNSRecord{}
	if err := json.Unmarshal(body, &result); err != nil {
		return nil, fmt.Errorf("CF API parse error: %w", err)
	}

	if !result.Success {
		msg := "unknown error"
		if len(result.Errors) > 0 {
			msg = result.Errors[0].Message
		}
		return nil, fmt.Errorf("CF API error: %s", msg)
	}

	records := result.Result.(*[]DNSRecord)
	for _, r := range *records {
		if r.Content == target {
			return &r, nil
		}
	}
	return nil, nil
}

// DeleteDNSRecord deletes a DNS record by its ID.
func (c *CFClient) DeleteDNSRecord(zoneID, recordID string) error {
	req, _ := http.NewRequest("DELETE", baseURL+"/zones/"+zoneID+"/dns_records/"+recordID, nil)
	req.Header.Set("Authorization", "Bearer "+c.Token)

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return fmt.Errorf("CF API request failed: %w", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	var result cfResponse
	if err := json.Unmarshal(body, &result); err != nil {
		return fmt.Errorf("CF API parse error: %w", err)
	}

	if !result.Success {
		msg := "unknown error"
		if len(result.Errors) > 0 {
			msg = result.Errors[0].Message
		}
		return fmt.Errorf("CF API error: %s", msg)
	}
	return nil
}
