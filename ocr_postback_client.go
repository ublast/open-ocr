package ocrworker

import (
	"bytes"
	"encoding/json"
	"github.com/couchbaselabs/logg"
	"io/ioutil"
	"net/http"
	"time"
)

var postTimeout = time.Duration(15 * time.Second)

type OcrPostClient struct {
}

func NewOcrPostClient() *OcrPostClient {
	return &OcrPostClient{}
}

func (c *OcrPostClient) postOcrRequest(ocrResult *OcrResult, replyToAddress string, numTry uint8) error {
	logg.LogTo("OCR_HTTP", "Post response called")
	logg.LogTo("OCR_HTTP", "sending for %d time the ocr to: %s ", numTry, replyToAddress)

	jsonReply, err := json.Marshal(ocrResult)
	if err != nil {
		ocrResult.Status = "error"
	}

	req, err := http.NewRequest("POST", replyToAddress, bytes.NewBuffer(jsonReply))
	req.Close = true
	req.Header.Set("User-Agent", "open-ocr/1.5")
	req.Header.Set("X-Custom-Header", "automated reply")
	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{Timeout: postTimeout}
	resp, err := client.Do(req)
	if err != nil {
		logg.LogWarn("OCR_HTTP: ocr was not delivered. %s did not respond", replyToAddress)
		return err
	}
	defer resp.Body.Close()

	body, err := ioutil.ReadAll(resp.Body)
	header := resp.StatusCode
	if err != nil {
		logg.LogWarn("OCR_HTTP: ocr was probably not delivered. %s response body is empty", replyToAddress)
		return err
	}
	logg.LogTo("OCR_HTTP", "response code is %v from peer %v and the content upon ocr delivery %s: ", header, replyToAddress, string(body))
	return err
}
