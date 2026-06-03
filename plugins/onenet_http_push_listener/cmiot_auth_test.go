package onenet_http_push_listener

import (
	"crypto/hmac"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"
)

func TestBuildCMiotAuthorization_User(t *testing.T) {
	const (
		accessKey = "720e323b10d54d3a8cdbe5dd4e641052"
		res       = "userid/511932"
		et        = "1770000000"
	)

	auth, err := buildCMiotAuthorization(accessKey, res, et, "sha1")
	if err != nil {
		t.Fatalf("build auth failed: %v", err)
	}

	want := "version=2022-05-01&res=userid%2F511932&et=1770000000&method=sha1&sign=xRvkt1q2HU7quic8C8xb28%2BdrQ4%3D"
	if auth != want {
		t.Fatalf("auth mismatch\nwant: %s\n got: %s", want, auth)
	}
}

func TestCMiotDeviceList_Debug(t *testing.T) {
	userID := strings.TrimSpace(os.Getenv("CMIOT_USER_ID"))
	accessKey := strings.TrimSpace(os.Getenv("CMIOT_ACCESS_KEY"))
	productID := strings.TrimSpace(os.Getenv("CMIOT_PRODUCT_ID"))
	method := strings.TrimSpace(os.Getenv("CMIOT_SIGN_METHOD"))
	if method == "" {
		method = "sha1"
	}

	if userID == "" || accessKey == "" || productID == "" {
		t.Skip("set env: CMIOT_USER_ID, CMIOT_ACCESS_KEY, CMIOT_PRODUCT_ID")
	}

	res := fmt.Sprintf("userid/%s", userID)
	et := fmt.Sprintf("%d", time.Now().Unix()+3600)
	auth, err := buildCMiotAuthorization(accessKey, res, et, method)
	if err != nil {
		t.Fatalf("build auth failed: %v", err)
	}

	endpoint := fmt.Sprintf("https://iot-api.heclouds.com/device/list?product_id=%s&offset=0&limit=10", url.QueryEscape(productID))
	req, err := http.NewRequest(http.MethodGet, endpoint, nil)
	if err != nil {
		t.Fatalf("new request failed: %v", err)
	}
	req.Header.Set("authorization", auth)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	t.Logf("http=%d body=%s", resp.StatusCode, string(body))

	var result struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
	}
	_ = json.Unmarshal(body, &result)

	if result.Code == 10403 {
		t.Fatalf("auth failed (10403): %s", result.Msg)
	}
}

func buildCMiotAuthorization(accessKey, res, et, method string) (string, error) {
	method = strings.ToLower(strings.TrimSpace(method))
	if method == "" {
		method = "sha1"
	}
	if method != "sha1" && method != "sha256" {
		return "", fmt.Errorf("unsupported method: %s", method)
	}

	key, err := base64.StdEncoding.DecodeString(strings.TrimSpace(accessKey))
	if err != nil {
		return "", fmt.Errorf("base64 decode access_key failed: %w", err)
	}

	version := "2022-05-01"
	strForSign := et + "\n" + method + "\n" + res + "\n" + version

	var digest []byte
	switch method {
	case "sha1":
		h := hmac.New(sha1.New, key)
		_, _ = h.Write([]byte(strForSign))
		digest = h.Sum(nil)
	case "sha256":
		h := hmac.New(sha256.New, key)
		_, _ = h.Write([]byte(strForSign))
		digest = h.Sum(nil)
	}

	sign := base64.StdEncoding.EncodeToString(digest)
	return fmt.Sprintf(
		"version=%s&res=%s&et=%s&method=%s&sign=%s",
		version,
		url.QueryEscape(res),
		et,
		method,
		url.QueryEscape(sign),
	), nil
}
