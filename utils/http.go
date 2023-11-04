package utils

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
)

func PostJson(url string, body []byte) (*http.Response, error) {
	resp, err := http.Post(url, "application/json", bytes.NewBuffer(body))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return resp, fmt.Errorf("Server response: %v", resp.Status)
	}
	return resp, nil
}

func PrintJsonResponse(resp io.ReadCloser) {
	defer resp.Close()
	body, _ := io.ReadAll(resp)

	// print indented JSON
	var out bytes.Buffer
	json.Indent(&out, body, "", "\t")
	out.WriteTo(os.Stdout)
}

func PrintErrorResponse(resp io.ReadCloser) {
	defer resp.Close()
	body, err := io.ReadAll(resp)
	if err != nil {
		fmt.Println("Error reading response:", err)
		return
	}

	// Convert the []byte to a string
	bodyStr := string(body)
	if bodyStr == "" {
		return
	}
	// Replace "\n" with actual newline characters
	formatted := strings.ReplaceAll(bodyStr, "\\n", "\n")
	formatted2 := strings.ReplaceAll(formatted, "\"", "")
	formatted3 := strings.ReplaceAll(formatted2, "{", "\n")
	formatted4 := strings.ReplaceAll(formatted3, "}", "")

	fmt.Print(formatted4)
}

func GetJsonResponse(resp io.ReadCloser) string {
	defer resp.Close()
	body, _ := io.ReadAll(resp)

	var out bytes.Buffer
	json.Indent(&out, body, "", "\t")
	return out.String()
}
