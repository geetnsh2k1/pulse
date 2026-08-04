package sqs

import (
	"encoding/json"
	"fmt"
	"strconv"

	"pulse/internal/awsfacade"
)

// Do dispatches one AWS-JSON-protocol action (the façade routes
// X-Amz-Target: AmazonSQS.<Action> here).
func (s *Service) Do(action string, body []byte) (any, *awsfacade.APIError) {
	switch action {
	case "SendMessage":
		return s.doSend(body)
	case "SendMessageBatch":
		return s.doSendBatch(body)
	case "ReceiveMessage":
		return s.doReceive(body)
	case "DeleteMessage":
		return s.doDelete(body)
	case "DeleteMessageBatch":
		return s.doDeleteBatch(body)
	case "ChangeMessageVisibility":
		return s.doChangeVisibility(body)
	case "GetQueueUrl":
		return s.doGetQueueURL(body)
	case "CreateQueue":
		return s.doCreateQueue(body)
	case "ListQueues":
		return s.doListQueues()
	case "PurgeQueue":
		return s.doPurge(body)
	case "GetQueueAttributes":
		return s.doGetAttributes(body)
	}
	return nil, &awsfacade.APIError{
		Type:      "com.amazonaws.sqs#UnsupportedOperation",
		QueryCode: "AWS.SimpleQueueService.UnsupportedOperation",
		Message:   fmt.Sprintf("pulse does not implement SQS %s yet — open an issue if you need it", action),
	}
}

func decode[T any](body []byte) (*T, *awsfacade.APIError) {
	var req T
	if len(body) > 0 {
		if err := json.Unmarshal(body, &req); err != nil {
			return nil, &awsfacade.APIError{
				Type:      "com.amazonaws.sqs#InvalidParameterValue",
				QueryCode: "InvalidParameterValue",
				Message:   "request body is not valid JSON: " + err.Error(),
			}
		}
	}
	return &req, nil
}

type sendRequest struct {
	QueueUrl          string
	MessageBody       string
	DelaySeconds      int
	MessageAttributes map[string]MessageAttribute
}

func (s *Service) doSend(body []byte) (any, *awsfacade.APIError) {
	req, apiErr := decode[sendRequest](body)
	if apiErr != nil {
		return nil, apiErr
	}
	id, apiErr := s.Send(queueFromURL(req.QueueUrl), req.MessageBody, req.DelaySeconds, req.MessageAttributes)
	if apiErr != nil {
		return nil, apiErr
	}
	resp := map[string]any{
		"MessageId":        id,
		"MD5OfMessageBody": md5Hex(req.MessageBody),
	}
	if len(req.MessageAttributes) > 0 {
		resp["MD5OfMessageAttributes"] = md5OfAttributes(req.MessageAttributes)
	}
	return resp, nil
}

type sendBatchRequest struct {
	QueueUrl string
	Entries  []struct {
		Id                string
		MessageBody       string
		DelaySeconds      int
		MessageAttributes map[string]MessageAttribute
	}
}

func (s *Service) doSendBatch(body []byte) (any, *awsfacade.APIError) {
	req, apiErr := decode[sendBatchRequest](body)
	if apiErr != nil {
		return nil, apiErr
	}
	queue := queueFromURL(req.QueueUrl)
	successful := []map[string]any{}
	failed := []map[string]any{}
	for _, e := range req.Entries {
		id, sendErr := s.Send(queue, e.MessageBody, e.DelaySeconds, e.MessageAttributes)
		if sendErr != nil {
			failed = append(failed, map[string]any{
				"Id": e.Id, "SenderFault": true, "Code": sendErr.QueryCode, "Message": sendErr.Message,
			})
			continue
		}
		entry := map[string]any{"Id": e.Id, "MessageId": id, "MD5OfMessageBody": md5Hex(e.MessageBody)}
		if len(e.MessageAttributes) > 0 {
			entry["MD5OfMessageAttributes"] = md5OfAttributes(e.MessageAttributes)
		}
		successful = append(successful, entry)
	}
	return map[string]any{"Successful": successful, "Failed": failed}, nil
}

type receiveRequest struct {
	QueueUrl            string
	MaxNumberOfMessages int
	VisibilityTimeout   int
	WaitTimeSeconds     int
}

func (s *Service) doReceive(body []byte) (any, *awsfacade.APIError) {
	req, apiErr := decode[receiveRequest](body)
	if apiErr != nil {
		return nil, apiErr
	}
	wait := req.WaitTimeSeconds
	if wait > 20 {
		wait = 20
	}
	msgs, apiErr := s.Receive(queueFromURL(req.QueueUrl), req.MaxNumberOfMessages, req.VisibilityTimeout, wait)
	if apiErr != nil {
		return nil, apiErr
	}
	if len(msgs) == 0 {
		return map[string]any{}, nil // AWS omits Messages when empty
	}
	out := make([]map[string]any, 0, len(msgs))
	for _, m := range msgs {
		entry := map[string]any{
			"MessageId":     m.ID,
			"ReceiptHandle": m.Receipt,
			"Body":          m.Body,
			"MD5OfBody":     md5Hex(m.Body),
			"Attributes": map[string]string{
				"ApproximateReceiveCount":          strconv.Itoa(m.ReceiveCount),
				"SentTimestamp":                    strconv.FormatInt(m.SentAt, 10),
				"ApproximateFirstReceiveTimestamp": strconv.FormatInt(m.FirstReceived, 10),
				"SenderId":                         "000000000000",
			},
		}
		if len(m.Attributes) > 0 {
			entry["MessageAttributes"] = m.Attributes
			entry["MD5OfMessageAttributes"] = md5OfAttributes(m.Attributes)
		}
		out = append(out, entry)
	}
	return map[string]any{"Messages": out}, nil
}

type deleteRequest struct {
	QueueUrl      string
	ReceiptHandle string
}

func (s *Service) doDelete(body []byte) (any, *awsfacade.APIError) {
	req, apiErr := decode[deleteRequest](body)
	if apiErr != nil {
		return nil, apiErr
	}
	if apiErr := s.Delete(queueFromURL(req.QueueUrl), req.ReceiptHandle); apiErr != nil {
		return nil, apiErr
	}
	return map[string]any{}, nil
}

type deleteBatchRequest struct {
	QueueUrl string
	Entries  []struct {
		Id            string
		ReceiptHandle string
	}
}

func (s *Service) doDeleteBatch(body []byte) (any, *awsfacade.APIError) {
	req, apiErr := decode[deleteBatchRequest](body)
	if apiErr != nil {
		return nil, apiErr
	}
	queue := queueFromURL(req.QueueUrl)
	successful := []map[string]any{}
	failed := []map[string]any{}
	for _, e := range req.Entries {
		if delErr := s.Delete(queue, e.ReceiptHandle); delErr != nil {
			failed = append(failed, map[string]any{
				"Id": e.Id, "SenderFault": true, "Code": delErr.QueryCode, "Message": delErr.Message,
			})
			continue
		}
		successful = append(successful, map[string]any{"Id": e.Id})
	}
	return map[string]any{"Successful": successful, "Failed": failed}, nil
}

type changeVisibilityRequest struct {
	QueueUrl          string
	ReceiptHandle     string
	VisibilityTimeout int
}

func (s *Service) doChangeVisibility(body []byte) (any, *awsfacade.APIError) {
	req, apiErr := decode[changeVisibilityRequest](body)
	if apiErr != nil {
		return nil, apiErr
	}
	if apiErr := s.ChangeVisibility(queueFromURL(req.QueueUrl), req.ReceiptHandle, req.VisibilityTimeout); apiErr != nil {
		return nil, apiErr
	}
	return map[string]any{}, nil
}

type queueNameRequest struct {
	QueueName string
}

func (s *Service) doGetQueueURL(body []byte) (any, *awsfacade.APIError) {
	req, apiErr := decode[queueNameRequest](body)
	if apiErr != nil {
		return nil, apiErr
	}
	if req.QueueName == "" {
		return nil, errInvalidParam("QueueName is required")
	}
	s.ensureForWrite(req.QueueName)
	return map[string]any{"QueueUrl": s.QueueURL(req.QueueName)}, nil
}

func (s *Service) doCreateQueue(body []byte) (any, *awsfacade.APIError) {
	req, apiErr := decode[queueNameRequest](body)
	if apiErr != nil {
		return nil, apiErr
	}
	if req.QueueName == "" {
		return nil, errInvalidParam("QueueName is required")
	}
	s.EnsureQueue(req.QueueName)
	return map[string]any{"QueueUrl": s.QueueURL(req.QueueName)}, nil
}

func (s *Service) doListQueues() (any, *awsfacade.APIError) {
	urls := []string{}
	for _, name := range s.QueueNames() {
		urls = append(urls, s.QueueURL(name))
	}
	return map[string]any{"QueueUrls": urls}, nil
}

type queueURLRequest struct {
	QueueUrl string
}

func (s *Service) doPurge(body []byte) (any, *awsfacade.APIError) {
	req, apiErr := decode[queueURLRequest](body)
	if apiErr != nil {
		return nil, apiErr
	}
	if apiErr := s.Purge(queueFromURL(req.QueueUrl)); apiErr != nil {
		return nil, apiErr
	}
	return map[string]any{}, nil
}

func (s *Service) doGetAttributes(body []byte) (any, *awsfacade.APIError) {
	req, apiErr := decode[queueURLRequest](body)
	if apiErr != nil {
		return nil, apiErr
	}
	name := queueFromURL(req.QueueUrl)
	st, apiErr := s.QueueStats(name)
	if apiErr != nil {
		return nil, apiErr
	}
	attrs := map[string]string{
		"ApproximateNumberOfMessages":           strconv.Itoa(st.Visible),
		"ApproximateNumberOfMessagesNotVisible": strconv.Itoa(st.InFlight),
		"ApproximateNumberOfMessagesDelayed":    strconv.Itoa(st.Delayed),
		"QueueArn":                              s.QueueARN(name),
	}
	if st.DLQ != "" {
		q := s.queue(name)
		redrive, _ := json.Marshal(map[string]any{
			"deadLetterTargetArn": s.QueueARN(st.DLQ),
			"maxReceiveCount":     q.maxReceiveCount,
		})
		attrs["RedrivePolicy"] = string(redrive)
	}
	return map[string]any{"Attributes": attrs}, nil
}
