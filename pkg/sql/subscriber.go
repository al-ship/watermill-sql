package sql

import (
	"context"
	"database/sql"
	"sync"
	"time"

	"github.com/oklog/ulid"
	"github.com/pkg/errors"

	"github.com/ThreeDotsLabs/watermill"
	"github.com/ThreeDotsLabs/watermill/message"
)

var (
	ErrSubscriberClosed = errors.New("subscriber is closed")
)

type SubscriberConfig struct {
	ConsumerGroup string

	// PollInterval is the interval to wait between subsequent SELECT queries, if no more messages were found in the database (Prefer using the BackoffManager instead).
	// Must be non-negative. Defaults to 1s.
	PollInterval time.Duration

	// ResendInterval is the time to wait before resending a nacked message.
	// Must be non-negative. Defaults to 1s.
	ResendInterval time.Duration

	// RetryInterval is the time to wait before resuming querying for messages after an error (Prefer using the BackoffManager instead).
	// Must be non-negative. Defaults to 1s.
	RetryInterval time.Duration

	// BackoffManager defines how how much to backoff when receiving errors.
	BackoffManager BackoffManager

	// SchemaAdapter provides the schema-dependent queries and arguments for them, based on topic/message etc.
	SchemaAdapter SchemaAdapter

	// OffsetsAdapter provides mechanism for saving acks and offsets of consumers.
	OffsetsAdapter OffsetsAdapter

	// InitializeSchema option enables initializing schema on making subscription.
	InitializeSchema bool
}

func (c *SubscriberConfig) setDefaults() {
	if c.PollInterval == 0 {
		c.PollInterval = time.Second
	}
	if c.ResendInterval == 0 {
		c.ResendInterval = time.Second
	}
	if c.RetryInterval == 0 {
		c.RetryInterval = time.Second
	}
	if c.BackoffManager == nil {
		c.BackoffManager = NewDefaultBackoffManager(c.PollInterval, c.RetryInterval)
	}
}

func (c SubscriberConfig) validate() error {
	if c.PollInterval <= 0 {
		return errors.New("poll interval must be a positive duration")
	}
	if c.ResendInterval <= 0 {
		return errors.New("resend interval must be a positive duration")
	}
	if c.RetryInterval <= 0 {
		return errors.New("resend interval must be a positive duration")
	}
	if c.SchemaAdapter == nil {
		return errors.New("schema adapter is nil")
	}
	if c.OffsetsAdapter == nil {
		return errors.New("offsets adapter is nil")
	}

	return nil
}

// Subscriber makes SELECT queries on the chosen table with the interval defined in the config.
// The rows are unmarshaled into Watermill messages.
type Subscriber struct {
	consumerIdBytes  []byte
	consumerIdString string

	db     beginner
	config SubscriberConfig

	subscribeWg *sync.WaitGroup
	closing     chan struct{}
	closed      bool

	logger watermill.LoggerAdapter
}

func NewSubscriber(db beginner, config SubscriberConfig, logger watermill.LoggerAdapter) (*Subscriber, error) {
	if db == nil {
		return nil, errors.New("db is nil")
	}
	config.setDefaults()
	err := config.validate()
	if err != nil {
		return nil, errors.Wrap(err, "invalid config")
	}

	if logger == nil {
		logger = watermill.NopLogger{}
	}

	idBytes, idStr, err := newSubscriberID()
	if err != nil {
		return &Subscriber{}, errors.Wrap(err, "cannot generate subscriber id")
	}
	logger = logger.With(watermill.LogFields{"subscriber_id": idStr})

	sub := &Subscriber{
		consumerIdBytes:  idBytes,
		consumerIdString: idStr,

		db:     db,
		config: config,

		subscribeWg: &sync.WaitGroup{},
		closing:     make(chan struct{}),

		logger: logger,
	}

	return sub, nil
}

func newSubscriberID() ([]byte, string, error) {
	id := watermill.NewULID()
	idBytes, err := ulid.MustParseStrict(id).MarshalBinary()
	if err != nil {
		return nil, "", errors.Wrap(err, "cannot marshal subscriber id")
	}

	return idBytes, id, nil
}

func (s *Subscriber) Subscribe(ctx context.Context, topic string) (o <-chan *message.Message, err error) {
	if s.closed {
		return nil, ErrSubscriberClosed
	}

	if err = validateTopicName(topic); err != nil {
		return nil, err
	}

	if s.config.InitializeSchema {
		if err := s.SubscribeInitialize(topic); err != nil {
			return nil, err
		}
	}

	// the information about closing the subscriber is propagated through ctx
	ctx, cancel := context.WithCancel(ctx)
	out := make(chan *message.Message)

	s.subscribeWg.Add(1)
	go func() {
		s.consume(ctx, topic, out)
		close(out)
		cancel()
	}()

	return out, nil
}

func (s *Subscriber) consume(ctx context.Context, topic string, out chan *message.Message) {
	defer s.subscribeWg.Done()

	logger := s.logger.With(watermill.LogFields{
		"topic":          topic,
		"consumer_group": s.config.ConsumerGroup,
	})

	var sleepTime time.Duration = 0
	for {
		select {
		case <-s.closing:
			logger.Info("Discarding queued message, subscriber closing", nil)
			return

		case <-ctx.Done():
			logger.Info("Stopping consume, context canceled", nil)
			return

		case <-time.After(sleepTime): // Wait if needed
			sleepTime = 0
		}

		messageUUID, noMsg, err := s.query(ctx, topic, out, logger)
		logger = logger.With(watermill.LogFields{"message_uuid": messageUUID})
		backoff := s.config.BackoffManager.HandleError(logger, noMsg, err)
		if backoff != 0 {
			if err != nil {
				logger = logger.With(watermill.LogFields{"err": err.Error()})
			}
			logger.Trace("Backing off querying", watermill.LogFields{
				"wait_time": backoff,
			})
		}
		sleepTime = backoff
	}
}

func (s *Subscriber) query(
	ctx context.Context,
	topic string,
	out chan *message.Message,
	logger watermill.LoggerAdapter,
) (messageUUID string, noMsg bool, err error) {
	txOptions := &sql.TxOptions{
		Isolation: sql.LevelRepeatableRead,
	}
	tx, err := s.db.BeginTx(ctx, txOptions)
	if err != nil {
		return "", false, errors.Wrap(err, "could not begin tx for querying")
	}

	defer func() {
		if err != nil {
			rollbackErr := tx.Rollback()
			if rollbackErr != nil && rollbackErr != sql.ErrTxDone {
				logger.Error("could not rollback tx for querying message", rollbackErr, nil)
			}
		} else {
			commitErr := tx.Commit()
			if commitErr != nil && commitErr != sql.ErrTxDone {
				logger.Error("could not commit tx for querying message", commitErr, nil)
			}
		}
	}()

	selectQuery, selectQueryArgs := s.config.SchemaAdapter.SelectQuery(
		topic,
		s.config.ConsumerGroup,
		s.config.OffsetsAdapter,
	)
	logger.Trace("Querying message", watermill.LogFields{
		"query":      selectQuery,
		"query_args": sqlArgsToLog(selectQueryArgs),
	})
	row := tx.QueryRowContext(ctx, selectQuery, selectQueryArgs...)

	offset, msg, err := s.config.SchemaAdapter.UnmarshalMessage(row)
	if errors.Cause(err) == sql.ErrNoRows {
		return "", true, nil
	} else if err != nil {
		return "", false, errors.Wrap(err, "could not unmarshal message from query")
	}

	logger = logger.With(watermill.LogFields{
		"msg_uuid": msg.UUID,
	})
	logger.Trace("Received message", nil)

	consumedQuery, consumedArgs := s.config.OffsetsAdapter.ConsumedMessageQuery(
		topic,
		offset,
		s.config.ConsumerGroup,
		s.consumerIdBytes,
	)
	if consumedQuery != "" {
		logger.Trace("Executing query to confirm message consumed", watermill.LogFields{
			"query":      consumedQuery,
			"query_args": sqlArgsToLog(consumedArgs),
		})

		_, err := tx.ExecContext(ctx, consumedQuery, consumedArgs...)
		if err != nil {
			return msg.UUID, false, errors.Wrap(err, "cannot send consumed query")
		}
	}

	msgCtx := setTxToContext(ctx, tx)

	acked := s.sendMessage(msgCtx, msg, out, logger)
	if acked {
		ackQuery, ackArgs := s.config.OffsetsAdapter.AckMessageQuery(topic, offset, s.config.ConsumerGroup)

		logger.Trace("Executing ack message query", watermill.LogFields{
			"query":      ackQuery,
			"query_args": sqlArgsToLog(ackArgs),
		})

		result, err := tx.ExecContext(ctx, ackQuery, ackArgs...)
		if err != nil {
			return msg.UUID, false, errors.Wrap(err, "could not get args for acking the message")
		}

		rowsAffected, _ := result.RowsAffected()

		logger.Trace("Executed ack message query", watermill.LogFields{
			"rows_affected": rowsAffected,
		})
	}

	return msg.UUID, false, nil
}

// sendMessages sends messages on the output channel.
func (s *Subscriber) sendMessage(
	ctx context.Context,
	msg *message.Message,
	out chan *message.Message,
	logger watermill.LoggerAdapter,
) (acked bool) {
	msgCtx, cancel := context.WithCancel(ctx)
	msg.SetContext(msgCtx)
	defer cancel()

ResendLoop:
	for {

		select {
		case out <- msg:

		case <-s.closing:
			logger.Info("Discarding queued message, subscriber closing", nil)
			return false

		case <-ctx.Done():
			logger.Info("Discarding queued message, context canceled", nil)
			return false
		}

		select {
		case <-msg.Acked():
			logger.Debug("Message acked by subscriber", nil)
			return true

		case <-msg.Nacked():
			//message nacked, try resending
			logger.Debug("Message nacked, resending", nil)
			msg = msg.Copy()
			msg.SetContext(msgCtx)

			if s.config.ResendInterval != 0 {
				time.Sleep(s.config.ResendInterval)
			}

			continue ResendLoop

		case <-s.closing:
			logger.Info("Discarding queued message, subscriber closing", nil)
			return false

		case <-ctx.Done():
			logger.Info("Discarding queued message, context canceled", nil)
			return false
		}
	}
}

func (s *Subscriber) Close() error {
	if s.closed {
		return nil
	}

	s.closed = true

	close(s.closing)
	s.subscribeWg.Wait()

	return nil
}

func (s *Subscriber) SubscribeInitialize(topic string) error {
	return initializeSchema(
		context.Background(),
		topic,
		s.logger,
		s.db,
		s.config.SchemaAdapter,
		s.config.OffsetsAdapter,
	)
}
