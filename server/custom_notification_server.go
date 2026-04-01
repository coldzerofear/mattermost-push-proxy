// Copyright (c) 2015 Mattermost, Inc. All Rights Reserved.
// See License.txt for license information.

package server

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/mattermost/mattermost/server/public/shared/mlog"
)

const (
	customMessageTypeDefault = "2"
	customReasonBadResponse  = "bad_response"
	customReasonRequestError = "request_error"
)

type CustomNotificationServer struct {
	settings     CustomPushSettings
	logger       *mlog.Logger
	metrics      *metrics
	sendTimeout  time.Duration
	retryTimeout time.Duration
	httpClient   *http.Client
}

type customMessageEnvelope struct {
	SysHead   customSysHead     `json:"sysHead"`
	TradeHead map[string]any    `json:"tradeHead"`
	Body      customMessageBody `json:"body"`
}

type customSysHead struct {
	LangCode string `json:"langCode"`
	PageNum  int    `json:"pageNum"`
	PageSize int    `json:"pageSize"`
}

type customMessageBody struct {
	Title         string `json:"title"`
	Content       string `json:"content"`
	Sender        string `json:"sender"`
	SenderName    string `json:"senderName"`
	SysID         string `json:"sysId"`
	ReceiverCodes string `json:"recieverCodes"`
	MessageType   string `json:"messageType"`
	IsSubApp      string `json:"isSubApp,omitempty"`
	IsToWeb       string `json:"isToWeb,omitempty"`
}

type customMessageResponse struct {
	Head struct {
		ResponseCode string `json:"responseCode"`
		ResponseMes  string `json:"responseMes"`
	} `json:"head"`
	Body struct {
		Success   bool   `json:"success"`
		ResultMsg string `json:"resultMsg"`
		EventNo   string `json:"eventNo"`
	} `json:"body"`
}

func NewCustomNotificationServer(settings CustomPushSettings, logger *mlog.Logger, m *metrics, sendTimeoutSec, retryTimeoutSec int) *CustomNotificationServer {
	return &CustomNotificationServer{
		settings:     settings,
		logger:       logger,
		metrics:      m,
		sendTimeout:  time.Duration(sendTimeoutSec) * time.Second,
		retryTimeout: time.Duration(retryTimeoutSec) * time.Second,
	}
}

func (me *CustomNotificationServer) Initialize() error {
	if me.settings.Type == "" {
		return fmt.Errorf("custom push type is required")
	}
	if me.settings.Endpoint == "" {
		return fmt.Errorf("custom push endpoint is required for type %s", me.settings.Type)
	}
	if me.settings.SysID == "" {
		return fmt.Errorf("custom push sysId is required for type %s", me.settings.Type)
	}
	if me.settings.MessageType == "" {
		me.settings.MessageType = customMessageTypeDefault
	}
	if me.settings.IsSubApp == "" {
		me.settings.IsSubApp = "0"
	}
	if me.settings.IsToWeb == "" {
		me.settings.IsToWeb = "0"
	}
	if strings.HasPrefix(me.settings.Endpoint, "https") {
		me.httpClient = &http.Client{
			Timeout: me.retryTimeout,
			Transport: &http.Transport{
				TLSClientConfig: &tls.Config{
					InsecureSkipVerify: true,
				},
			}}
	} else {
		me.httpClient = &http.Client{Timeout: me.retryTimeout}
	}

	return nil
}

func (me *CustomNotificationServer) SendNotification(msg *PushNotification) PushResponse {
	pushType := msg.Type
	if pushType == "" {
		pushType = PushTypeMessage
	}

	if me.metrics != nil {
		me.metrics.incrementNotificationTotal(me.settings.Type, pushType)
	}

	if pushType != PushTypeMessage && pushType != PushTypeSession && pushType != PushTypeTest {
		me.logger.Debug("Skipping unsupported custom push type", mlog.String("type", pushType), mlog.String("platform", me.settings.Type))
		if me.metrics != nil {
			if msg.AckID != "" {
				me.metrics.incrementSuccessWithAck(me.settings.Type, pushType)
			} else {
				me.metrics.incrementSuccess(me.settings.Type, pushType)
			}
		}
		return NewOkPushResponse()
	}

	receiverCode := strings.TrimSpace(msg.DeviceID)
	if receiverCode == "" {
		if me.metrics != nil {
			me.metrics.incrementFailure(me.settings.Type, pushType, customReasonRequestError)
		}
		return NewErrorPushResponse("missing receiver code")
	}

	payload := customMessageEnvelope{
		SysHead: customSysHead{
			LangCode: "zh_cn",
			PageNum:  1,
			PageSize: 10,
		},
		TradeHead: map[string]any{},
		Body: customMessageBody{
			Title:         me.resolveTitle(msg),
			Content:       msg.Message,
			Sender:        me.resolveSender(msg),
			SenderName:    me.resolveSenderName(msg),
			SysID:         me.settings.SysID,
			ReceiverCodes: receiverCode,
			MessageType:   me.settings.MessageType,
			IsSubApp:      me.settings.IsSubApp,
			IsToWeb:       me.settings.IsToWeb,
		},
	}

	resp, err := me.sendWithRetry(payload)
	if err != nil {
		me.logger.Error("Failed to send custom push", mlog.String("sid", msg.ServerID), mlog.String("did", msg.DeviceID), mlog.String("type", me.settings.Type), mlog.Err(err))
		if me.metrics != nil {
			me.metrics.incrementFailure(me.settings.Type, pushType, customReasonRequestError)
		}
		return NewErrorPushResponse(err.Error())
	}

	if !resp.Body.Success {
		reason := resp.Body.ResultMsg
		if reason == "" {
			reason = resp.Head.ResponseMes
		}
		if reason == "" {
			reason = customReasonBadResponse
		}
		me.logger.Error("Custom push responded with failure", mlog.String("type", me.settings.Type), mlog.String("did", msg.DeviceID), mlog.String("reason", reason))
		if me.metrics != nil {
			me.metrics.incrementFailure(me.settings.Type, pushType, reason)
		}
		return NewErrorPushResponse(reason)
	}

	if me.metrics != nil {
		if msg.AckID != "" {
			me.metrics.incrementSuccessWithAck(me.settings.Type, pushType)
		} else {
			me.metrics.incrementSuccess(me.settings.Type, pushType)
		}
	}

	return NewOkPushResponse()
}

func (me *CustomNotificationServer) sendWithRetry(payload customMessageEnvelope) (*customMessageResponse, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}

	generalContext, cancel := context.WithTimeout(context.Background(), me.sendTimeout)
	defer cancel()

	var lastErr error
	waitTime := time.Second
	for retries := 0; retries < MAX_RETRIES; retries++ {
		start := time.Now()
		retryContext, cancelRetry := context.WithTimeout(generalContext, me.retryTimeout)
		resp, err := me.sendOnce(retryContext, body)
		cancelRetry()
		if me.metrics != nil {
			me.metrics.observerNotificationResponse(me.settings.Type, time.Since(start).Seconds())
		}
		if err == nil {
			return resp, nil
		}
		lastErr = err

		if retries == MAX_RETRIES-1 || generalContext.Err() != nil {
			break
		}

		select {
		case <-generalContext.Done():
		case <-time.After(waitTime):
		}
	}

	return nil, lastErr
}

func (me *CustomNotificationServer) sendOnce(ctx context.Context, body []byte) (*customMessageResponse, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, me.settings.Endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	resp, err := me.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("custom push returned status %d: %s", resp.StatusCode, strings.TrimSpace(string(respBody)))
	}

	var parsed customMessageResponse
	if err := json.Unmarshal(respBody, &parsed); err != nil {
		return nil, err
	}

	return &parsed, nil
}

func (me *CustomNotificationServer) resolveTitle(msg *PushNotification) string {
	if msg.ChannelName != "" {
		return msg.ChannelName
	}
	if me.settings.Title != "" {
		return me.settings.Title
	}
	return "Mattermost"
}

func (me *CustomNotificationServer) resolveSender(msg *PushNotification) string {
	if msg.SenderUsername != "" {
		return msg.SenderUsername
	}
	if msg.SenderID != "" {
		return msg.SenderID
	}
	return "mattermost"
}

func (me *CustomNotificationServer) resolveSenderName(msg *PushNotification) string {
	if msg.SenderName != "" {
		return msg.SenderName
	}
	if msg.SenderUsername != "" {
		return msg.SenderUsername
	}
	if msg.SenderID != "" {
		return msg.SenderID
	}
	return "Mattermost"
}
