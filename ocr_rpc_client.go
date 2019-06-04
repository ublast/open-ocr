package ocrworker

import (
	"encoding/json"
	"fmt"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
	"github.com/sasha-s/go-deadlock"
	"github.com/streadway/amqp"
	"os"
	"time"
)

var (
	// RPCResponseTimeout sets timeout for getting the result from channel
	RPCResponseTimeout = time.Second * 20
	// ResponseCacheTimeout sets global timeout in seconds for request
	// engine will be killed after reaching the time limit, user will get timeout error
	ResponseCacheTimeout uint = 240
	// check interval for request to be ready
	tickerWithPostActionInterval = time.Second * 2
)

type OcrRpcClient struct {
	rabbitConfig RabbitConfig
	connection   *amqp.Connection
	channel      *amqp.Channel
}

type OcrResult struct {
	Text   string `json:"text"`
	Status string `json:"status"`
	ID     string `json:"id"`
}

func newOcrResult(id string) OcrResult {
	ocrResult := &OcrResult{}
	ocrResult.Status = "processing"
	ocrResult.ID = id
	return *ocrResult
}

var (
	requestsAndTimersMu deadlock.Mutex
	requests            = make(map[string]chan OcrResult)
	timers              = make(map[string]*time.Timer)
)
var (
	numRetries uint8 = 3
)

func NewOcrRpcClient(rc RabbitConfig) (*OcrRpcClient, error) {
	ocrRpcClient := &OcrRpcClient{
		rabbitConfig: rc,
	}
	return ocrRpcClient, nil
}

// DecodeImage is the main function to do a ocr on incoming request.
// It's handling the parameter and the whole workflow
func (c *OcrRpcClient) DecodeImage(ocrRequest OcrRequest, requestID string) (OcrResult, error) {
	var err error

	logger := zerolog.New(os.Stdout).With().
		Str("RequestID", requestID).Timestamp().Logger()

	logger.Info().Str("component", "OCR_CLIENT").
		Bool("Deferred", ocrRequest.Deferred).
		Str("DocType", ocrRequest.DocType).
		Interface("EngineArgs", ocrRequest.EngineArgs).
		Bool("InplaceDecode", ocrRequest.InplaceDecode).
		Uint16("PageNumber", ocrRequest.PageNumber).
		Str("ReplyTo", ocrRequest.ReplyTo).
		Str("UserAgent", ocrRequest.UserAgent).
		Str("EngineType", string(ocrRequest.EngineType)).
		Uint("TimeOut", ocrRequest.TimeOut).
		Msg("incoming request")

	if ocrRequest.ReplyTo != "" {
		logger.Info().Str("component", "OCR_CLIENT").Msg("Automated response requested")
		validURL, err := checkURLForReplyTo(ocrRequest.ReplyTo)
		if err != nil {
			return OcrResult{}, err
		}
		ocrRequest.ReplyTo = validURL
		// force set the deferred flag to drop the connection and deliver
		// ocr automatically to the URL in ReplyTo tag
		ocrRequest.Deferred = true
	}

	var messagePriority uint8 = 1
	if ocrRequest.DocType != "" {
		logger.Info().Str("component", "OCR_CLIENT").Str("DocType", ocrRequest.DocType).
			Msg("message type is specified, check for higher prio request")
		// set highest priority for defined message id
		// TODO do not hard code DocType priority
		if ocrRequest.DocType == "egvp" {
			messagePriority = 9
		}

	}
	// setting the timeout for worker if not set or to high
	if ocrRequest.TimeOut >= uint(3600) || ocrRequest.TimeOut == 0 {
		ocrRequest.TimeOut = ResponseCacheTimeout
	}

	// setting rabbitMQ correlation ID. There is no reason to be different from requestID
	correlationUUID := requestID
	logger.Info().Str("component", "OCR_CLIENT").Str("DocType", ocrRequest.DocType).
		Str("AmqpURI", c.rabbitConfig.AmqpURI).
		Msg("dialing RabbitMQ")

	c.connection, err = amqp.Dial(c.rabbitConfig.AmqpURI)
	if err != nil {
		return OcrResult{Text: "Internal Server Error: message broker is not reachable", Status: "error"}, err
	}
	// if we close the connection here, the deferred status wont get the ocr result
	// and will be always returning "processing"
	// defer c.connection.Close()

	c.channel, err = c.connection.Channel()
	if err != nil {
		return OcrResult{}, err
	}

	if err := c.channel.ExchangeDeclare(
		c.rabbitConfig.Exchange,     // name
		c.rabbitConfig.ExchangeType, // type
		true,                        // durable
		false,                       // auto-deleted
		false,                       // internal
		false,                       // noWait
		nil,                         // arguments
	); err != nil {
		return OcrResult{}, err
	}

	rpcResponseChan := make(chan OcrResult)

	callbackQueue, err := c.subscribeCallbackQueue(correlationUUID, rpcResponseChan)
	if err != nil {
		return OcrResult{}, err
	}

	// Reliable publisher confirms require confirm.select support from the
	// connection.
	if c.rabbitConfig.Reliable {
		if err := c.channel.Confirm(false); err != nil {
			return OcrResult{}, err
		}

		ack, nack := c.channel.NotifyConfirm(make(chan uint64, 1), make(chan uint64, 1))

		defer confirmDelivery(ack, nack)
	}

	// TODO: we only need to download image url if there are
	// any preprocessors.  if rabbitmq isn't in same data center
	// as open-ocr, it will be expensive in terms of bandwidth
	// to have image binary in messages
	if ocrRequest.ImgBytes == nil {

		// if we do not have bytes use base 64 file by converting it to bytes
		if ocrRequest.hasBase64() {
			logger.Info().Str("component", "OCR_CLIENT").Msg("OCR request has base 64 convert it to bytes")

			err = ocrRequest.decodeBase64()
			if err != nil {
				logger.Warn().Str("component", "OCR_CLIENT").
					Err(err).
					Msg("Error decoding base64")
				return OcrResult{}, err
			}
		} else {
			// if we do not have base 64 or bytes download the file
			err = ocrRequest.downloadImgUrl()
			if err != nil {
				logger.Warn().Str("component", "OCR_CLIENT").
					Err(err).
					Msg("Error downloading img url")
				return OcrResult{}, err
			}
		}
	}

	routingKey := ocrRequest.nextPreprocessor(c.rabbitConfig.RoutingKey)
	logger.Info().Str("component", "OCR_CLIENT").Str("routingKey", routingKey).
		Msg("publishing with routing key")

	ocrRequestJson, err := json.Marshal(ocrRequest)
	if err != nil {
		return OcrResult{}, err
	}
	if err = c.channel.Publish(
		c.rabbitConfig.Exchange, // publish to an exchange
		routingKey,
		false, // mandatory
		false, // immediate
		amqp.Publishing{
			Headers:         amqp.Table{},
			ContentType:     "application/json",
			ContentEncoding: "",
			Body:            []byte(ocrRequestJson),
			DeliveryMode:    amqp.Transient,  // 1=non-persistent, 2=persistent
			Priority:        messagePriority, // 0-9
			ReplyTo:         callbackQueue.Name,
			CorrelationId:   correlationUUID,
			// a bunch of application/implementation-specific fields
		},
	); err != nil {
		return OcrResult{}, nil
	}
	// TODO rewrite postClient to not check the status, just give it an ocrRequest of file
	// TODO rewrite it also check if there are memory leak after global timeout
	// TODO on deffered request if you get request by polling before it was
	// TODO automaticaly delivered then atomatic deliver will POST empty request back after timeout
	if ocrRequest.Deferred {
		logger.Info().Str("component", "OCR_CLIENT").Msg("Asynchronous request accepted")
		timer := time.NewTimer(time.Duration(ResponseCacheTimeout) * time.Second)
		logger.Debug().Str("component", "OCR_CLIENT").Msg("locking vrequestsAndTimersMu")
		requestsAndTimersMu.Lock()
		requests[requestID] = rpcResponseChan
		timers[requestID] = timer
		logger.Debug().Str("component", "OCR_CLIENT").Msg("unlocking vrequestsAndTimersMu")
		requestsAndTimersMu.Unlock()
		// deferred == true but no automatic reply to the requester
		// client should poll to get the ocr
		if ocrRequest.ReplyTo == "" {
			// thi go routine will cancel the request after global timeout if client stopped polling
			go func() {
				<-timer.C
				CheckOcrStatusByID(requestID)
			}()
			return OcrResult{
				ID:     requestID,
				Status: "processing",
			}, nil
		}
		// automatic delivery oder POST to the requester
		// check interval for order to be ready to deliver
		tickerWithPostAction := time.NewTicker(tickerWithPostActionInterval)

		go func() {
		T:
			for {
				select {
				case t := <-tickerWithPostAction.C:
					logger.Info().Str("component", "OCR_CLIENT").
						Str("time", t.String()).
						Msg("checking for request to be done")

					ocrRes, err := CheckOcrStatusByID(requestID)
					if err != nil {
						logger.Error().Err(err)
					} // only if status is done end the goroutine. otherwise continue polling
					if ocrRes.Status == "done" || ocrRes.Status == "error" {
						logger.Info().Str("component", "OCR_CLIENT").
							Msg("request is ready")

						var tryCounter uint8 = 1
						ocrPostClient := newOcrPostClient()
						for ok := true; ok; ok = tryCounter <= numRetries {
							err = ocrPostClient.postOcrRequest(&ocrRes, ocrRequest.ReplyTo, tryCounter)
							if err != nil {
								tryCounter++
								logger.Error().Err(err)
								time.Sleep(2 * time.Second)
							} else {
								break
							}
						}
						tickerWithPostAction.Stop()
						break T
					}
				}
			}
		}()
		// initial response to the caller to inform it with request id
		return OcrResult{
			ID:     requestID,
			Status: "processing",
		}, nil
	} else {
		// in case it works, error checking is needed
		return CheckOcrStatusByID(requestID)
	}
}

func (c OcrRpcClient) subscribeCallbackQueue(correlationUUID string, rpcResponseChan chan OcrResult) (amqp.Queue, error) {

	queueArgs := make(amqp.Table)
	queueArgs["x-max-priority"] = uint8(10)

	// declare a callback queue where we will receive rpc responses
	callbackQueue, err := c.channel.QueueDeclare(
		"",        // name -- let rabbit generate a random one
		false,     // durable
		true,      // delete when unused
		true,      // exclusive
		false,     // noWait
		queueArgs, // arguments
	)
	if err != nil {
		return amqp.Queue{}, err
	}

	// bind the callback queue to an exchange + routing key
	if err = c.channel.QueueBind(
		callbackQueue.Name,      // name of the queue
		callbackQueue.Name,      // bindingKey
		c.rabbitConfig.Exchange, // sourceExchange
		false,                   // noWait
		queueArgs,               // arguments
	); err != nil {
		return amqp.Queue{}, err
	}

	log.Info().Str("component", "OCR_CLIENT").Str("callbackQueue", callbackQueue.Name)

	deliveries, err := c.channel.Consume(
		callbackQueue.Name, // name
		tag,                // consumerTag,
		true,               // noAck
		true,               // exclusive
		false,              // noLocal
		false,              // noWait
		queueArgs,          // arguments
	)
	if err != nil {
		return amqp.Queue{}, err
	}

	go c.handleRpcResponse(deliveries, correlationUUID, rpcResponseChan)

	return callbackQueue, nil

}

func (c OcrRpcClient) handleRpcResponse(deliveries <-chan amqp.Delivery, correlationUuid string, rpcResponseChan chan OcrResult) {
	// correlationUuid is the same as RequestID
	logger := zerolog.New(os.Stdout).With().
		Str("RequestID", correlationUuid).Timestamp().Logger()
	logger.Info().Str("component", "OCR_CLIENT").Msg("looping over deliveries...:")
	// TODO this defer is probably a memory leak
	// defer c.connection.Close()
	for d := range deliveries {
		if d.CorrelationId == correlationUuid {
			bodyLenToLog := len(d.Body)
			defer c.connection.Close()
			if bodyLenToLog > 32 {
				bodyLenToLog = 32
			}
			logger.Info().Str("component", "OCR_CLIENT").
				Int("size", len(d.Body)).
				Uint64("DeliveryTag", d.DeliveryTag).
				Hex("payload(32 Bytes)", d.Body[0:bodyLenToLog]).
				Str("ReplyTo", d.ReplyTo).
				Msg("got delivery")

			ocrResult := OcrResult{}
			err := json.Unmarshal(d.Body, &ocrResult)
			if err != nil {
				msg := "Error unmarshalling json: %v.  Error: %v"
				errMsg := fmt.Sprintf(msg, string(d.Body[0:bodyLenToLog]), err)
				logger.Error().Err(fmt.Errorf(errMsg))
			}
			ocrResult.ID = correlationUuid

			logger.Info().Str("component", "OCR_CLIENT").Msg("send result to rpcResponseChan")
			rpcResponseChan <- ocrResult
			logger.Info().Str("component", "OCR_CLIENT").Msg("sent result to rpcResponseChan")
			return

		} else {
			logger.Info().Str("component", "OCR_CLIENT").Str("CorrelationId", d.CorrelationId).
				Msg("ignoring delivery w/ correlation id")
		}

	}
}

func CheckOcrStatusByID(requestID string) (OcrResult, error) {
	log.Debug().Str("component", "OCR_CLIENT").Msg("locking vrequestsAndTimersMu CheckOcrStatusByID")
	requestsAndTimersMu.Lock()
	if _, ok := requests[requestID]; !ok {
		log.Debug().Str("component", "OCR_CLIENT").Msg("unlocking vrequestsAndTimersMu with id mismatch CheckOcrStatusByID")
		requestsAndTimersMu.Unlock()
		return OcrResult{}, fmt.Errorf("no such request %s", requestID)
	}
	//ocrResult, err := CheckReply(requests[requestID], time.Duration(timeoutForCheckReply)*time.Second)

	log.Info().Str("component", "OCR_CLIENT").Msg("checking for response ")

	ocrResult := <-requests[requestID]
	if ocrResult.Status != "processing" {
		log.Debug().Str("component", "OCR_CLIENT").Msg("deleting requests and timers")
		delete(requests, requestID)
		timers[requestID].Stop()
		delete(timers, requestID)
	}
	ocrResult.ID = requestID
	log.Debug().Str("component", "OCR_CLIENT").Msg("unlocking vrequestsAndTimersMu CheckOcrStatusByID")
	requestsAndTimersMu.Unlock()
	return ocrResult, nil
}

func confirmDelivery(ack, nack chan uint64) {
	select {
	case tag := <-ack:
		log.Info().Str("component", "OCR_CLIENT").
			Uint64("tag", tag).
			Msg("confirmed delivery with tag")
	case tag := <-nack:
		log.Info().Str("component", "OCR_CLIENT").
			Uint64("tag", tag).
			Msg("failed to confirm delivery")
	}
}
