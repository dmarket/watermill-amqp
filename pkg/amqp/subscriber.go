package amqp

import (
	"context"
	"sync"
	"time"

	"github.com/hashicorp/go-multierror"

	"github.com/ThreeDotsLabs/watermill"
	"github.com/ThreeDotsLabs/watermill/message"
	"github.com/pkg/errors"
	"github.com/streadway/amqp"
)

type Subscriber struct {
	*connectionWrapper

	config Config
}

func NewSubscriber(config Config, logger watermill.LoggerAdapter) (*Subscriber, error) {
	if err := config.ValidateSubscriber(); err != nil {
		return nil, err
	}

	conn, err := newConnection(config, logger)
	if err != nil {
		return nil, err
	}

	return &Subscriber{conn, config}, nil
}

// Subscribe consumes messages from AMQP broker.
//
// Watermill's topic in Subscribe is not mapped to AMQP's topic, but depending on configuration it can be mapped
// to exchange, queue or routing key.
// For detailed description of nomenclature mapping, please check "Nomenclature" paragraph in doc.go file.
func (s *Subscriber) Subscribe(ctx context.Context, topic string) (<-chan *message.Message, error) {
	if s.closed {
		return nil, errors.New("pub/sub is closed")
	}

	if !s.IsConnected() {
		return nil, errors.New("not connected to AMQP")
	}

	logFields := watermill.LogFields{"topic": topic}

	out := make(chan *message.Message, 0)

	queueName := s.config.Queue.GenerateName(topic)
	logFields["amqp_queue_name"] = queueName

	exchangeName := s.config.Exchange.GenerateName(topic)
	logFields["amqp_exchange_name"] = exchangeName

	if err := s.prepareConsume(queueName, exchangeName, logFields); err != nil {
		return nil, errors.Wrap(err, "failed to prepare consume")
	}

	s.subscribingWg.Add(1)
	go func(ctx context.Context) {
		defer func() {
			close(out)
			s.logger.Info("Stopped consuming from AMQP channel", logFields)
			s.subscribingWg.Done()
		}()

	ReconnectLoop:
		for {
			s.logger.Debug("Waiting for s.connected or s.closing in ReconnectLoop", logFields)

			select {
			case <-s.connected:
				s.logger.Debug("Connection established in ReconnectLoop", logFields)
				// runSubscriber blocks until connection fails or Close() is called
				s.runSubscriber(ctx, out, queueName, exchangeName, logFields)
			case <-s.closing:
				s.logger.Debug("Stopping ReconnectLoop (closing)", logFields)
				break ReconnectLoop
			case <-ctx.Done():
				s.logger.Debug("Stopping ReconnectLoop (ctx done)", logFields)
				break ReconnectLoop
			}

			time.Sleep(time.Millisecond * 100)
		}
	}(ctx)

	return out, nil
}

func (s *Subscriber) SubscribeInitialize(topic string) (err error) {
	if s.closed {
		return errors.New("pub/sub is closed")
	}

	if !s.IsConnected() {
		return errors.New("not connected to AMQP")
	}

	logFields := watermill.LogFields{"topic": topic}

	queueName := s.config.Queue.GenerateName(topic)
	logFields["amqp_queue_name"] = queueName

	exchangeName := s.config.Exchange.GenerateName(topic)
	logFields["amqp_exchange_name"] = exchangeName

	s.logger.Info("Initializing subscribe", logFields)

	return errors.Wrap(s.prepareConsume(queueName, exchangeName, logFields), "failed to prepare consume")
}

func (s *Subscriber) prepareConsume(queueName string, exchangeName string, logFields watermill.LogFields) (err error) {
	channel, err := s.openSubscribeChannel(logFields)
	if err != nil {
		return err
	}
	defer func() {
		if channelCloseErr := channel.Close(); channelCloseErr != nil {
			err = multierror.Append(err, channelCloseErr)
		}
	}()

	if err = s.config.TopologyBuilder.BuildTopology(channel, queueName, exchangeName, s.config, s.logger); err != nil {
		return err
	}

	s.logger.Debug("Queue bound to exchange", logFields)

	return nil
}

func (s *Subscriber) runSubscriber(
	ctx context.Context,
	out chan *message.Message,
	queueName string,
	exchangeName string,
	logFields watermill.LogFields,
) {
	channel, err := s.openSubscribeChannel(logFields)
	if err != nil {
		s.logger.Error("Failed to open channel", err, logFields)
		return
	}
	defer func() {
		err := channel.Close()
		s.logger.Error("Failed to close channel", err, logFields)
	}()

	notifyCloseChannel := channel.NotifyClose(make(chan *amqp.Error))

	sub := subscription{
		out:                out,
		logFields:          logFields,
		notifyCloseChannel: notifyCloseChannel,
		channel:            channel,
		queueName:          queueName,
		logger:             s.logger,
		closing:            s.closing,
		config:             s.config,
	}

	s.logger.Info("Starting consuming from AMQP channel", logFields)

	sub.ProcessMessages(ctx)
}

func (s *Subscriber) openSubscribeChannel(logFields watermill.LogFields) (*amqp.Channel, error) {
	if !s.IsConnected() {
		return nil, errors.New("not connected to AMQP")
	}

	channel, err := s.amqpConnection.Channel()
	if err != nil {
		return nil, errors.Wrap(err, "cannot open channel")
	}
	s.logger.Debug("Channel opened", logFields)

	if s.config.Consume.Qos != (QosConfig{}) {
		err = channel.Qos(
			s.config.Consume.Qos.PrefetchCount,
			s.config.Consume.Qos.PrefetchSize,
			s.config.Consume.Qos.Global,
		)
		s.logger.Debug("Qos set", logFields)
	}

	return channel, nil
}

type subscription struct {
	out                chan *message.Message
	logFields          watermill.LogFields
	notifyCloseChannel chan *amqp.Error
	channel            *amqp.Channel
	queueName          string

	logger  watermill.LoggerAdapter
	closing chan struct{}
	config  Config
}

// undelivered represents message that wasn't processed
// due to error or subscription issues and must be Nack`ed.
// Error represents only fail reason, not Nack indicator.
type undelivered struct {
	amqp.Delivery
	error
}

func (s *subscription) ProcessMessages(ctx context.Context) {
	amqpMsgs, err := s.createConsumer(s.queueName, s.channel)
	if err != nil {
		s.logger.Error("Failed to start consuming messages", err, s.logFields)
		return
	}

	// unproc collects unprocessed deliveries
	unproc := make(chan undelivered, cap(amqpMsgs)+1) // +1 for close attempt on full buffer
	// errbreak breaks ConsumingLoop on unexpected error
	errbreak := make(chan error, 1) // 1 for write attempt on exited ConsumingLoop
	// done determines whether all listeners are done
	done := make(chan struct{})

	// any undelivered message will be Nack`ed
	// regardless to its error value.
	go func() {
		for del := range unproc {
			if del.error != nil {
				s.logger.Error("Processing message failed, sending nack", del.error, s.logFields)
			} else {
				s.logger.Info("Message wasn't processed, sending nack", s.logFields)
			}

			if err := s.nackMsg(del.Delivery); err != nil {
				s.logger.Error("Cannot nack message", err, s.logFields)

				// something went really wrong when we cannot nack, let's reconnect
				errbreak <- err
				break
			}
		}
		close(done)
	}()

	// wip waits till all processing messages aren't handled
	var wip sync.WaitGroup

ConsumingLoop:
	for {
		select {
		case amqpMsg := <-amqpMsgs:
			wip.Add(1)
			s.processMessage(ctx, amqpMsg, s.out, unproc, &wip, s.logFields)
			continue ConsumingLoop

		case <-s.notifyCloseChannel:
			s.logger.Error("Channel closed, stopping ProcessMessages", nil, s.logFields)
			break ConsumingLoop

		case <-s.closing:
			s.logger.Info("Closing from Subscriber received", s.logFields)
			break ConsumingLoop

		case <-ctx.Done():
			s.logger.Info("Closing from ctx received", s.logFields)
			break ConsumingLoop

		case err := <-errbreak:
			s.logger.Error("Something went wrong, stopping ProcessMessages", err, s.logFields)
			break ConsumingLoop
		}
	}

	wip.Wait()

	close(unproc)
	<-done
}

func (s *subscription) createConsumer(queueName string, channel *amqp.Channel) (<-chan amqp.Delivery, error) {
	amqpMsgs, err := channel.Consume(
		queueName,
		s.config.Consume.Consumer,
		false, // autoAck must be set to false - acks are managed by Watermill
		s.config.Consume.Exclusive,
		s.config.Consume.NoLocal,
		s.config.Consume.NoWait,
		s.config.Consume.Arguments,
	)
	if err != nil {
		return nil, errors.Wrap(err, "cannot consume from channel")
	}

	return amqpMsgs, nil
}

func (s *subscription) processMessage(
	ctx context.Context,
	amqpMsg amqp.Delivery,
	out chan *message.Message,
	unproc chan<- undelivered,
	wg *sync.WaitGroup,
	logFields watermill.LogFields,
) {
	candef := true
	defer doif(&candef, wg.Done)

	msg, err := s.config.Marshaler.Unmarshal(amqpMsg)
	if err != nil {
		unproc <- undelivered{Delivery: amqpMsg, error: err}
		return
	}

	ctx, cancelCtx := context.WithCancel(ctx)
	msg.SetContext(ctx)
	defer doif(&candef, cancelCtx)

	msgLogFields := logFields.Add(watermill.LogFields{"message_uuid": msg.UUID})
	s.logger.Trace("Unmarshaled message", msgLogFields)

	select {
	case <-s.closing:
		s.logger.Info("Message not consumed, pub/sub is closing", msgLogFields)

		unproc <- undelivered{Delivery: amqpMsg}
		return
	case out <- msg:
		s.logger.Trace("Message sent to consumer", msgLogFields)
	}

	// now all deferred funcs will be maintained by goroutine
	candef = false

	// async message Ack/Nack handling allows unblock
	// receiving of rest messages and process them simultaneously.
	go func() {
		defer cancelCtx()
		defer wg.Done()

		var err error
		select {
		case <-s.closing:
			s.logger.Trace("Closing pub/sub, message discarded before ack", msgLogFields)
			err = s.nackMsg(amqpMsg)
		case <-msg.Acked():
			s.logger.Trace("Message Acked", msgLogFields)
			err = amqpMsg.Ack(false)
		case <-msg.Nacked():
			s.logger.Trace("Message Nacked", msgLogFields)
			err = s.nackMsg(amqpMsg)
		}
		if err != nil {
			unproc <- undelivered{Delivery: amqpMsg, error: err}
			return
		}
	}()
}

// doif is suitable for deferred execution func
// that authorizes closure execution with
// given cond.
func doif(cond *bool, f func()) {
	if cond == nil {
		return
	}
	if !*cond {
		return
	}
	f()
}

func (s *subscription) nackMsg(amqpMsg amqp.Delivery) error {
	return amqpMsg.Nack(false, !s.config.Consume.NoRequeueOnNack)
}
