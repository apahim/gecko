package postgres

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strconv"
	"sync"
	"time"

	"github.com/lib/pq"

	"github.com/openshift-online/gecko/orlop/pkg/apiserver/storage"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"sigs.k8s.io/controller-runtime/pkg/client"
)

// PostgresBroadcaster implements storage.EventBroadcaster using PostgreSQL LISTEN/NOTIFY
// and an event log table for historical event replay.
type PostgresBroadcaster struct {
	db          *sql.DB
	listener    *pq.Listener
	ctx         context.Context
	cancel      context.CancelFunc
	channelName string
	tableName   string
	scheme      *runtime.Scheme
	gvk         schema.GroupVersionKind

	mu          sync.RWMutex
	subscribers map[int]chan storage.ResourceEvent
	nextID      int
	closed      bool
}

// PostgresBroadcasterConfig configures the SQL broadcaster.
type PostgresBroadcasterConfig struct {
	DB          *sql.DB
	ConnString  string                  // For pq.Listener
	ChannelName string                  // LISTEN/NOTIFY channel name
	TableName   string                  // Event log table name
	Scheme      *runtime.Scheme         // Required for object deserialization
	GVK         schema.GroupVersionKind // Required for setting TypeMeta
}

// NewPostgresBroadcaster creates a broadcaster backed by PostgreSQL.
// Uses LISTEN/NOTIFY for real-time event distribution and a table for historical replay.
func NewPostgresBroadcaster(ctx context.Context, config PostgresBroadcasterConfig) (*PostgresBroadcaster, error) {
	if config.DB == nil {
		return nil, fmt.Errorf("database connection is required")
	}
	if config.ConnString == "" {
		return nil, fmt.Errorf("connection string is required for listener")
	}

	channelName := config.ChannelName
	if channelName == "" {
		channelName = "resource_events"
	}

	tableName := config.TableName
	if tableName == "" {
		tableName = "event_log"
	}

	bCtx, cancel := context.WithCancel(ctx)

	b := &PostgresBroadcaster{
		db:          config.DB,
		ctx:         bCtx,
		cancel:      cancel,
		channelName: channelName,
		tableName:   tableName,
		scheme:      config.Scheme,
		gvk:         config.GVK,
		subscribers: make(map[int]chan storage.ResourceEvent),
	}

	// Create event log table
	if err := b.createEventLogTable(); err != nil {
		cancel()
		return nil, fmt.Errorf("failed to create event log table: %w", err)
	}

	// Create listener for NOTIFY events
	listener := pq.NewListener(
		config.ConnString,
		10*time.Second, // minReconnectInterval
		time.Minute,    // maxReconnectInterval
		func(ev pq.ListenerEventType, err error) {
			if err != nil {
				// Log connection issues
				fmt.Printf("Listener event: %v, error: %v\n", ev, err)
			}
		},
	)

	if err := listener.Listen(channelName); err != nil {
		cancel()
		return nil, fmt.Errorf("failed to listen on channel %s: %w", channelName, err)
	}

	b.listener = listener

	// Start listening for notifications
	go b.listenForNotifications()

	return b, nil
}

// createEventLogTable creates the table for storing event history.
func (b *PostgresBroadcaster) createEventLogTable() error {
	schema := fmt.Sprintf(`
		CREATE TABLE IF NOT EXISTS %s (
			id BIGSERIAL PRIMARY KEY,
			event_type VARCHAR(20) NOT NULL,
			resource_version BIGINT NOT NULL,
			object_data JSONB NOT NULL,
			created_at TIMESTAMP NOT NULL DEFAULT NOW()
		);

		CREATE INDEX IF NOT EXISTS idx_%s_resource_version ON %s(resource_version);
		CREATE INDEX IF NOT EXISTS idx_%s_created_at ON %s(created_at);
	`, b.tableName,
		b.tableName, b.tableName,
		b.tableName, b.tableName)

	_, err := b.db.ExecContext(context.Background(), schema)
	return err
}

// listenForNotifications listens for PostgreSQL NOTIFY messages.
func (b *PostgresBroadcaster) listenForNotifications() {
	for {
		select {
		case <-b.ctx.Done():
			return
		case notification := <-b.listener.Notify:
			if notification == nil {
				continue
			}

			// Load the event from the event log by id
			event, err := b.loadEvent(notification.Extra)
			if err != nil {
				fmt.Printf("Failed to load event %s: %v\n", notification.Extra, err)
				continue
			}

			// Broadcast to subscribers
			b.broadcastToSubscribers(event)
		case <-time.After(90 * time.Second):
			// Ping to check connection
			go b.listener.Ping()
		}
	}
}

// loadEvent fetches a single event from the event log by its id.
func (b *PostgresBroadcaster) loadEvent(id string) (storage.ResourceEvent, error) {
	query := fmt.Sprintf(`
		SELECT event_type, resource_version, object_data
		FROM %s WHERE id = $1
	`, b.tableName)

	var eventType string
	var resourceVersion int64
	var objectData []byte
	err := b.db.QueryRowContext(b.ctx, query, id).Scan(&eventType, &resourceVersion, &objectData)
	if err != nil {
		return storage.ResourceEvent{}, fmt.Errorf("failed to query event: %w", err)
	}

	obj, err := b.reconstructObject(objectData)
	if err != nil {
		return storage.ResourceEvent{}, fmt.Errorf("failed to reconstruct object: %w", err)
	}

	obj.SetResourceVersion(strconv.FormatInt(resourceVersion, 10))

	return storage.ResourceEvent{
		Type:            storage.EventType(eventType),
		ResourceVersion: strconv.FormatInt(resourceVersion, 10),
		Object:          obj,
	}, nil
}

// reconstructObject deserializes JSON data into a typed client.Object using the scheme.
func (b *PostgresBroadcaster) reconstructObject(data []byte) (client.Object, error) {
	if b.scheme == nil || b.gvk.Empty() {
		// Fallback to unstructured if no scheme/GVK provided
		obj := &unstructured.Unstructured{}
		if err := json.Unmarshal(data, &obj.Object); err != nil {
			return nil, err
		}
		return obj, nil
	}

	// Create a new typed object from the scheme
	obj, err := b.scheme.New(b.gvk)
	if err != nil {
		// Type not registered in scheme, fallback to unstructured
		unstruct := &unstructured.Unstructured{}
		if err := json.Unmarshal(data, &unstruct.Object); err != nil {
			return nil, err
		}
		unstruct.SetGroupVersionKind(b.gvk)
		return unstruct, nil
	}

	// Unmarshal JSON directly into the typed object
	if err := json.Unmarshal(data, obj); err != nil {
		return nil, fmt.Errorf("failed to unmarshal into typed object: %w", err)
	}

	// Ensure GVK is set on the object
	obj.GetObjectKind().SetGroupVersionKind(b.gvk)

	// Assert that the object implements client.Object
	clientObj, ok := obj.(client.Object)
	if !ok {
		return nil, fmt.Errorf("object does not implement client.Object")
	}

	return clientObj, nil
}

// broadcastToSubscribers sends an event to all active subscribers.
func (b *PostgresBroadcaster) broadcastToSubscribers(event storage.ResourceEvent) {
	b.mu.RLock()
	defer b.mu.RUnlock()

	for _, ch := range b.subscribers {
		select {
		case ch <- event:
		default:
			// Don't block if subscriber is slow
		}
	}
}

// Broadcast implements storage.EventBroadcaster.
// Stores the event in the database and notifies all listeners via NOTIFY.
func (b *PostgresBroadcaster) Broadcast(event storage.ResourceEvent) {
	b.mu.RLock()
	if b.closed {
		b.mu.RUnlock()
		return
	}
	b.mu.RUnlock()

	ctx := context.Background()

	// Parse resource version
	rv, _ := strconv.ParseInt(event.ResourceVersion, 10, 64)

	// Serialize object, stripping resourceVersion from the JSON blob
	objectData, err := marshalData(event.Object)
	if err != nil {
		fmt.Printf("Failed to marshal object: %v\n", err)
		return
	}

	// Insert into event log and get the row ID
	query := fmt.Sprintf(`
		INSERT INTO %s (event_type, resource_version, object_data)
		VALUES ($1, $2, $3)
		RETURNING id
	`, b.tableName)

	var eventID int64
	err = b.db.QueryRowContext(ctx, query, event.Type, rv, objectData).Scan(&eventID)
	if err != nil {
		fmt.Printf("Failed to insert event: %v\n", err)
		return
	}

	// Send lightweight NOTIFY with just the event log row ID
	notifyQuery := fmt.Sprintf("NOTIFY %s, '%d'", b.channelName, eventID)
	_, err = b.db.ExecContext(ctx, notifyQuery)
	if err != nil {
		fmt.Printf("Failed to send NOTIFY: %v\n", err)
	}
}

// Subscribe implements storage.EventBroadcaster.
// Returns historical events since the requested resource version, then live events.
func (b *PostgresBroadcaster) Subscribe(sinceResourceVersion string) (<-chan storage.ResourceEvent, func(), error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	if b.closed {
		return nil, nil, fmt.Errorf("broadcaster is closed")
	}

	id := b.nextID
	b.nextID++

	ch := make(chan storage.ResourceEvent, 100)
	b.subscribers[id] = ch

	// Send historical events if requested (including from "0")
	if sinceResourceVersion != "" {
		go b.sendHistoricalEvents(ch, sinceResourceVersion)
	}

	// Create stop function
	stopFunc := func() {
		b.unsubscribe(id)
	}

	return ch, stopFunc, nil
}

// sendHistoricalEvents queries the event log for historical events.
func (b *PostgresBroadcaster) sendHistoricalEvents(ch chan storage.ResourceEvent, sinceResourceVersion string) {
	rv, err := strconv.ParseInt(sinceResourceVersion, 10, 64)
	if err != nil {
		return
	}

	query := fmt.Sprintf(`
		SELECT event_type, resource_version, object_data
		FROM %s
		WHERE resource_version > $1
		ORDER BY resource_version ASC
		LIMIT 1000
	`, b.tableName)

	rows, err := b.db.QueryContext(context.Background(), query, rv)
	if err != nil {
		fmt.Printf("Failed to query historical events: %v\n", err)
		return
	}
	defer rows.Close()

	for rows.Next() {
		var eventType string
		var resourceVersion int64
		var objectData []byte

		if err := rows.Scan(&eventType, &resourceVersion, &objectData); err != nil {
			fmt.Printf("Failed to scan event: %v\n", err)
			continue
		}

		// Reconstruct the typed object using the scheme
		obj, err := b.reconstructObject(objectData)
		if err != nil {
			fmt.Printf("Failed to reconstruct object: %v\n", err)
			continue
		}

		// Restore resourceVersion from the database column
		obj.SetResourceVersion(strconv.FormatInt(resourceVersion, 10))

		event := storage.ResourceEvent{
			Type:            storage.EventType(eventType),
			ResourceVersion: strconv.FormatInt(resourceVersion, 10),
			Object:          obj,
		}

		select {
		case ch <- event:
		default:
			// Channel full, stop sending historical events
			return
		}
	}
}

// unsubscribe removes a subscriber.
func (b *PostgresBroadcaster) unsubscribe(id int) {
	b.mu.Lock()
	defer b.mu.Unlock()

	if ch, exists := b.subscribers[id]; exists {
		close(ch)
		delete(b.subscribers, id)
	}
}

// Close implements storage.EventBroadcaster.
func (b *PostgresBroadcaster) Close() error {
	b.mu.Lock()
	defer b.mu.Unlock()

	if b.closed {
		return nil
	}

	b.closed = true
	b.cancel()

	// Close all subscriber channels
	for id, ch := range b.subscribers {
		close(ch)
		delete(b.subscribers, id)
	}

	// Close listener
	if b.listener != nil {
		return b.listener.Close()
	}

	return nil
}

// PruneOldEvents removes events older than the specified duration.
// Should be called periodically to prevent unbounded growth of event log.
func (b *PostgresBroadcaster) PruneOldEvents(ctx context.Context, olderThan time.Duration) error {
	cutoff := time.Now().Add(-olderThan)

	query := fmt.Sprintf(`
		DELETE FROM %s
		WHERE created_at < $1
	`, b.tableName)

	result, err := b.db.ExecContext(ctx, query, cutoff)
	if err != nil {
		return fmt.Errorf("failed to prune events: %w", err)
	}

	rowsDeleted, _ := result.RowsAffected()
	fmt.Printf("Pruned %d old events\n", rowsDeleted)

	return nil
}

// Verify PostgresBroadcaster implements storage.EventBroadcaster and storage.EventPruner.
var (
	_ storage.EventBroadcaster = (*PostgresBroadcaster)(nil)
	_ storage.EventPruner      = (*PostgresBroadcaster)(nil)
)
