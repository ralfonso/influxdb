package influxdb

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/influxdb/influxdb/influxql"
	"github.com/influxdb/influxdb/messaging"
	"golang.org/x/crypto/bcrypt"
)

const (
	// DefaultRootPassword is the password initially set for the root user.
	// It is also used when reseting the root user's password.
	DefaultRootPassword = "root"

	// DefaultRetentionPolicyName is the name of a databases's default shard space.
	DefaultRetentionPolicyName = "default"

	// DefaultSplitN represents the number of partitions a shard is split into.
	DefaultSplitN = 1

	// DefaultReplicaN represents the number of replicas data is written to.
	DefaultReplicaN = 1

	// DefaultShardDuration is the time period held by a shard.
	DefaultShardDuration = 7 * (24 * time.Hour)

	// DefaultShardRetention is the length of time before a shard is dropped.
	DefaultShardRetention = 7 * (24 * time.Hour)
)

const (
	// Data node messages
	createDataNodeMessageType = messaging.MessageType(0x00)
	deleteDataNodeMessageType = messaging.MessageType(0x01)

	// Database messages
	createDatabaseMessageType = messaging.MessageType(0x10)
	deleteDatabaseMessageType = messaging.MessageType(0x11)

	// Retention policy messages
	createRetentionPolicyMessageType     = messaging.MessageType(0x20)
	updateRetentionPolicyMessageType     = messaging.MessageType(0x21)
	deleteRetentionPolicyMessageType     = messaging.MessageType(0x22)
	setDefaultRetentionPolicyMessageType = messaging.MessageType(0x23)

	// User messages
	createUserMessageType = messaging.MessageType(0x30)
	updateUserMessageType = messaging.MessageType(0x31)
	deleteUserMessageType = messaging.MessageType(0x32)

	// Shard messages
	createShardGroupIfNotExistsMessageType = messaging.MessageType(0x40)
	deleteShardGroupMessageType            = messaging.MessageType(0x41)

	// Measurement messages
	createMeasurementsIfNotExistsMessageType = messaging.MessageType(0x50)

	// Continuous Query messages
	createContinuousQueryMessageType = messaging.MessageType(0x60)

	// Write series data messages (per-topic)
	writeRawSeriesMessageType = messaging.MessageType(0x70)

	// Privilege messages
	setPrivilegeMessageType = messaging.MessageType(0x80)
)

// Server represents a collection of metadata and raw metric data.
type Server struct {
	mu     sync.RWMutex
	id     uint64
	path   string
	done   chan struct{} // goroutine close notification
	rpDone chan struct{} // retention policies goroutine close notification

	client MessagingClient  // broker client
	index  uint64           // highest broadcast index seen
	errors map[uint64]error // message errors

	meta *metastore // metadata store

	dataNodes map[uint64]*DataNode // data nodes by id
	databases map[string]*database // databases by name
	users     map[string]*User     // user by name

	shards           map[uint64]*Shard   // shards by shard id
	shardsBySeriesID map[uint32][]*Shard // shards by series id

	Logger *log.Logger

	authenticationEnabled bool

	// continuous query settings
	RecomputePreviousN     int
	RecomputeNoOlderThan   time.Duration
	ComputeRunsPerInterval int
	ComputeNoMoreThan      time.Duration

	// This is the last time this data node has run continuous queries.
	// Keep this state in memory so if a broker makes a request in another second
	// to compute, it won't rerun CQs that have already been run. If this data node
	// is just getting the request after being off duty for running CQs then
	// it will recompute all of them
	lastContinuousQueryRun time.Time
}

// NewServer returns a new instance of Server.
func NewServer() *Server {
	s := Server{
		meta:      &metastore{},
		errors:    make(map[uint64]error),
		dataNodes: make(map[uint64]*DataNode),
		databases: make(map[string]*database),
		users:     make(map[string]*User),

		shards:           make(map[uint64]*Shard),
		shardsBySeriesID: make(map[uint32][]*Shard),
		Logger:           log.New(os.Stderr, "[server] ", log.LstdFlags),
	}
	// Server will always return with authentication enabled.
	// This ensures that disabling authentication must be an explicit decision.
	// To set the server to 'authless mode', call server.SetAuthenticationEnabled(false).
	s.authenticationEnabled = true
	return &s
}

// SetAuthenticationEnabled turns on or off server authentication
func (s *Server) SetAuthenticationEnabled(enabled bool) {
	s.authenticationEnabled = enabled
}

// ID returns the data node id for the server.
// Returns zero if the server is closed or the server has not joined a cluster.
func (s *Server) ID() uint64 {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.id
}

// Index returns the index for the server.
func (s *Server) Index() uint64 {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.index
}

// Path returns the path used when opening the server.
// Returns an empty string when the server is closed.
func (s *Server) Path() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.path
}

// shardPath returns the path for a shard.
func (s *Server) shardPath(id uint64) string {
	if s.path == "" {
		return ""
	}
	return filepath.Join(s.path, "shards", strconv.FormatUint(id, 10))
}

// metaPath returns the path for the metastore.
func (s *Server) metaPath() string {
	if s.path == "" {
		return ""
	}
	return filepath.Join(s.path, "meta")
}

// SetLogOutput sets writer for all Server log output.
func (s *Server) SetLogOutput(w io.Writer) {
	s.Logger = log.New(w, "[server] ", log.LstdFlags)
}

// Open initializes the server from a given path.
func (s *Server) Open(path string) error {
	// Ensure the server isn't already open and there's a path provided.
	if s.opened() {
		return ErrServerOpen
	} else if path == "" {
		return ErrPathRequired
	}

	// Set the server path.
	s.path = path

	// Create required directories.
	if err := os.MkdirAll(path, 0700); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Join(path, "shards"), 0700); err != nil {
		return err
	}

	// Open metadata store.
	if err := s.meta.open(s.metaPath()); err != nil {
		return fmt.Errorf("meta: %s", err)
	}

	// Load state from metastore.
	if err := s.load(); err != nil {
		return fmt.Errorf("load: %s", err)
	}

	// TODO: Open shard data stores.
	// TODO: Associate series ids with shards.

	return nil
}

// opened returns true when the server is open.
func (s *Server) opened() bool { return s.path != "" }

// Close shuts down the server.
func (s *Server) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if !s.opened() {
		return ErrServerClosed
	}

	if s.rpDone != nil {
		close(s.rpDone)
	}

	// Remove path.
	s.path = ""

	// Close message processing.
	s.setClient(nil)

	// Close metastore.
	_ = s.meta.close()

	// Close shards.
	for _, sh := range s.shards {
		_ = sh.close()
	}

	return nil
}

// load reads the state of the server from the metastore.
func (s *Server) load() error {
	return s.meta.view(func(tx *metatx) error {
		// Read server id.
		s.id = tx.id()

		// Load data nodes.
		s.dataNodes = make(map[uint64]*DataNode)
		for _, node := range tx.dataNodes() {
			s.dataNodes[node.ID] = node
		}

		// Load databases.
		s.databases = make(map[string]*database)
		for _, db := range tx.databases() {
			s.databases[db.name] = db

			// load the index
			log.Printf("Loading metadata index for %s\n", db.name)
			err := s.meta.view(func(tx *metatx) error {
				tx.indexDatabase(db)
				return nil
			})
			if err != nil {
				return err
			}
		}

		// Open all shards.
		for _, db := range s.databases {
			for _, rp := range db.policies {
				for _, g := range rp.shardGroups {
					for _, sh := range g.Shards {
						if err := sh.open(s.shardPath(sh.ID)); err != nil {
							return fmt.Errorf("cannot open shard store: id=%d, err=%s", sh.ID, err)
						}
					}
				}
			}
		}

		// Load users.
		s.users = make(map[string]*User)
		for _, u := range tx.users() {
			s.users[u.Name] = u
		}

		return nil
	})
}

//  StartRetentionPolicyEnforcement launches retention policy enforcement.
func (s *Server) StartRetentionPolicyEnforcement(checkInterval time.Duration) error {
	if checkInterval == 0 {
		return fmt.Errorf("retention policy check interval must be non-zero")
	}
	rpDone := make(chan struct{}, 0)
	s.rpDone = rpDone
	go func() {
		for {
			select {
			case <-rpDone:
				return
			case <-time.After(checkInterval):
				s.EnforceRetentionPolicies()
			}
		}
	}()
	return nil
}

// EnforceRetentionPolicies ensures that data that is aging-out due to retention policies
// is removed from the server.
func (s *Server) EnforceRetentionPolicies() {
	log.Println("retention policy enforcement check commencing")

	// Check all shard groups.
	for _, db := range s.databases {
		for _, rp := range db.policies {
			for _, g := range rp.shardGroups {
				if g.EndTime.Add(rp.Duration).Before(time.Now()) {
					log.Printf("shard group %d, retention policy %s, database %s due for deletion",
						g.ID, rp.Name, db.name)
					if err := s.DeleteShardGroup(db.name, rp.Name, g.ID); err != nil {
						log.Printf("failed to request deletion of shard group %d: %s", g.ID, err.Error())
					}
				}
			}
		}
	}
}

// Client retrieves the current messaging client.
func (s *Server) Client() MessagingClient {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.client
}

// SetClient sets the messaging client on the server.
func (s *Server) SetClient(client MessagingClient) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.setClient(client)
}

func (s *Server) setClient(client MessagingClient) error {
	// Ensure the server is open.
	if !s.opened() {
		return ErrServerClosed
	}

	// Stop previous processor, if running.
	if s.done != nil {
		close(s.done)
		s.done = nil
	}

	// Set the messaging client.
	s.client = client

	// Start goroutine to read messages from the broker.
	if client != nil {
		done := make(chan struct{}, 0)
		s.done = done
		go s.processor(client, done)
	}

	return nil
}

// broadcast encodes a message as JSON and send it to the broker's broadcast topic.
// This function waits until the message has been processed by the server.
// Returns the broker log index of the message or an error.
func (s *Server) broadcast(typ messaging.MessageType, c interface{}) (uint64, error) {
	// Encode the command.
	data, err := json.Marshal(c)
	if err != nil {
		return 0, err
	}

	// Publish the message.
	m := &messaging.Message{
		Type:    typ,
		TopicID: messaging.BroadcastTopicID,
		Data:    data,
	}
	index, err := s.client.Publish(m)
	if err != nil {
		return 0, err
	}

	// Wait for the server to receive the message.
	err = s.Sync(index)

	return index, err
}

// Sync blocks until a given index (or a higher index) has been applied.
// Returns any error associated with the command.
func (s *Server) Sync(index uint64) error {
	for {
		// Check if index has occurred. If so, retrieve the error and return.
		s.mu.RLock()
		if s.index >= index {
			err, ok := s.errors[index]
			if ok {
				delete(s.errors, index)
			}
			s.mu.RUnlock()
			return err
		}
		s.mu.RUnlock()

		// Otherwise wait momentarily and check again.
		time.Sleep(1 * time.Millisecond)
	}
}

// Initialize creates a new data node and initializes the server's id to 1.
func (s *Server) Initialize(u *url.URL) error {
	// Create a new data node.
	if err := s.CreateDataNode(u); err != nil {
		return err
	}

	// Ensure the data node returns with an ID of 1.
	// If it doesn't then something went really wrong. We have to panic because
	// the messaging client relies on the first server being assigned ID 1.
	n := s.DataNodeByURL(u)
	assert(n != nil && n.ID == 1, "invalid initial server id: %d", n.ID)

	// Set the ID on the metastore.
	if err := s.meta.mustUpdate(func(tx *metatx) error {
		return tx.setID(n.ID)
	}); err != nil {
		return err
	}

	// Set the ID on the server.
	s.id = 1

	return nil
}

// This is the same struct we use in the httpd package, but
// it seems overkill to export it and share it
type dataNodeJSON struct {
	ID  uint64 `json:"id"`
	URL string `json:"url"`
}

// Join creates a new data node in an existing cluster, copies the metastore,
// and initializes the ID.
func (s *Server) Join(u *url.URL, joinURL *url.URL) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	// Encode data node request.
	var buf bytes.Buffer
	if err := json.NewEncoder(&buf).Encode(&dataNodeJSON{URL: u.String()}); err != nil {
		return err
	}

	// Send request.
	joinURL = copyURL(joinURL)
	joinURL.Path = "/data_nodes"
	resp, err := http.Post(joinURL.String(), "application/octet-stream", &buf)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	// Check if created.
	if resp.StatusCode != http.StatusCreated {
		return ErrUnableToJoin
	}

	// Decode response.
	var n dataNodeJSON
	if err := json.NewDecoder(resp.Body).Decode(&n); err != nil {
		return err
	}
	assert(n.ID > 0, "invalid join node id returned: %d", n.ID)

	// Download the metastore from joining server.
	joinURL.Path = "/metastore"
	resp, err = http.Get(joinURL.String())
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	// Check response & parse content length.
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("unsuccessful meta copy: status=%d (%s)", resp.StatusCode, joinURL.String())
	}
	sz, err := strconv.ParseInt(resp.Header.Get("Content-Length"), 10, 64)
	if err != nil {
		return fmt.Errorf("cannot parse meta size: %s", err)
	}

	// Close the metastore.
	_ = s.meta.close()

	// Overwrite the metastore.
	f, err := os.Create(s.metaPath())
	if err != nil {
		return fmt.Errorf("create meta file: %s", err)
	}

	// Copy and check size.
	if _, err := io.CopyN(f, resp.Body, sz); err != nil {
		_ = f.Close()
		return fmt.Errorf("copy meta file: %s", err)
	}
	_ = f.Close()

	// Reopen metastore.
	s.meta = &metastore{}
	if err := s.meta.open(s.metaPath()); err != nil {
		return fmt.Errorf("reopen meta: %s", err)
	}

	// Update the ID on the metastore.
	if err := s.meta.mustUpdate(func(tx *metatx) error {
		return tx.setID(n.ID)
	}); err != nil {
		return err
	}

	// Reload the server.
	log.Printf("reloading metadata")
	if err := s.load(); err != nil {
		return fmt.Errorf("reload: %s", err)
	}

	return nil
}

// CopyMetastore writes the underlying metastore data file to a writer.
func (s *Server) CopyMetastore(w io.Writer) error {
	return s.meta.mustView(func(tx *metatx) error {
		// Set content lengh if this is a HTTP connection.
		if w, ok := w.(http.ResponseWriter); ok {
			w.Header().Set("Content-Length", strconv.Itoa(int(tx.Size())))
		}

		// Write entire database to the writer.
		return tx.Copy(w)
	})
}

// DataNode returns a data node by id.
func (s *Server) DataNode(id uint64) *DataNode {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.dataNodes[id]
}

// DataNodeByURL returns a data node by url.
func (s *Server) DataNodeByURL(u *url.URL) *DataNode {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, n := range s.dataNodes {
		if n.URL.String() == u.String() {
			return n
		}
	}
	return nil
}

// DataNodes returns a list of data nodes.
func (s *Server) DataNodes() (a []*DataNode) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, n := range s.dataNodes {
		a = append(a, n)
	}
	sort.Sort(dataNodes(a))
	return
}

// CreateDataNode creates a new data node with a given URL.
func (s *Server) CreateDataNode(u *url.URL) error {
	c := &createDataNodeCommand{URL: u.String()}
	_, err := s.broadcast(createDataNodeMessageType, c)
	return err
}

func (s *Server) applyCreateDataNode(m *messaging.Message) (err error) {
	var c createDataNodeCommand
	mustUnmarshalJSON(m.Data, &c)

	s.mu.Lock()
	defer s.mu.Unlock()

	// Validate parameters.
	if c.URL == "" {
		return ErrDataNodeURLRequired
	}

	// Check that another node with the same URL doesn't already exist.
	u, _ := url.Parse(c.URL)
	for _, n := range s.dataNodes {
		if n.URL.String() == u.String() {
			return ErrDataNodeExists
		}
	}

	// Create data node.
	n := newDataNode()
	n.URL = u

	// Persist to metastore.
	err = s.meta.mustUpdate(func(tx *metatx) error {
		n.ID = tx.nextDataNodeID()
		return tx.saveDataNode(n)
	})

	// Add to node on server.
	s.dataNodes[n.ID] = n

	return
}

type createDataNodeCommand struct {
	URL string `json:"url"`
}

// DeleteDataNode deletes an existing data node.
func (s *Server) DeleteDataNode(id uint64) error {
	c := &deleteDataNodeCommand{ID: id}
	_, err := s.broadcast(deleteDataNodeMessageType, c)
	return err
}

func (s *Server) applyDeleteDataNode(m *messaging.Message) (err error) {
	var c deleteDataNodeCommand
	mustUnmarshalJSON(m.Data, &c)

	s.mu.Lock()
	defer s.mu.Unlock()
	n := s.dataNodes[c.ID]
	if n == nil {
		return ErrDataNodeNotFound
	}

	// Remove from metastore.
	err = s.meta.mustUpdate(func(tx *metatx) error { return tx.deleteDataNode(c.ID) })

	// Delete the node.
	delete(s.dataNodes, n.ID)

	return
}

type deleteDataNodeCommand struct {
	ID uint64 `json:"id"`
}

// DatabaseExists returns true if a database exists.
func (s *Server) DatabaseExists(name string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.databases[name] != nil
}

// Databases returns a sorted list of all database names.
func (s *Server) Databases() (a []string) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, db := range s.databases {
		a = append(a, db.name)
	}
	sort.Strings(a)
	return
}

// CreateDatabase creates a new database.
func (s *Server) CreateDatabase(name string) error {
	c := &createDatabaseCommand{Name: name}
	_, err := s.broadcast(createDatabaseMessageType, c)
	return err
}

func (s *Server) applyCreateDatabase(m *messaging.Message) (err error) {
	var c createDatabaseCommand
	mustUnmarshalJSON(m.Data, &c)

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.databases[c.Name] != nil {
		return ErrDatabaseExists
	}

	// Create database entry.
	db := newDatabase()
	db.name = c.Name

	// Persist to metastore.
	err = s.meta.mustUpdate(func(tx *metatx) error { return tx.saveDatabase(db) })

	// Add to databases on server.
	s.databases[c.Name] = db

	return
}

type createDatabaseCommand struct {
	Name string `json:"name"`
}

// DeleteDatabase deletes an existing database.
func (s *Server) DeleteDatabase(name string) error {
	c := &deleteDatabaseCommand{Name: name}
	_, err := s.broadcast(deleteDatabaseMessageType, c)
	return err
}

func (s *Server) applyDeleteDatabase(m *messaging.Message) (err error) {
	var c deleteDatabaseCommand
	mustUnmarshalJSON(m.Data, &c)

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.databases[c.Name] == nil {
		return ErrDatabaseNotFound
	}

	// Remove from metastore.
	err = s.meta.mustUpdate(func(tx *metatx) error { return tx.deleteDatabase(c.Name) })

	// Delete the database entry.
	delete(s.databases, c.Name)
	return
}

type deleteDatabaseCommand struct {
	Name string `json:"name"`
}

// Shard returns a shard by ID.
func (s *Server) Shard(id uint64) *Shard {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.shards[id]
}

// shardGroupByTimestamp returns a group for a database, policy & timestamp.
func (s *Server) shardGroupByTimestamp(database, policy string, timestamp time.Time) (*ShardGroup, error) {
	db := s.databases[database]
	if db == nil {
		return nil, ErrDatabaseNotFound
	}
	return db.shardGroupByTimestamp(policy, timestamp)
}

// ShardGroups returns a list of all shard groups for a database.
// Returns an error if the database doesn't exist.
func (s *Server) ShardGroups(database string) ([]*ShardGroup, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	// Lookup database.
	db := s.databases[database]
	if db == nil {
		return nil, ErrDatabaseNotFound
	}

	// Retrieve groups from database.
	var a []*ShardGroup
	for _, rp := range db.policies {
		for _, g := range rp.shardGroups {
			a = append(a, g)
		}
	}
	return a, nil
}

// CreateShardGroupIfNotExists creates the shard group for a retention policy for the interval a timestamp falls into.
func (s *Server) CreateShardGroupIfNotExists(database, policy string, timestamp time.Time) error {
	c := &createShardGroupIfNotExistsCommand{Database: database, Policy: policy, Timestamp: timestamp}
	_, err := s.broadcast(createShardGroupIfNotExistsMessageType, c)
	return err
}

func (s *Server) applyCreateShardGroupIfNotExists(m *messaging.Message) (err error) {
	var c createShardGroupIfNotExistsCommand
	mustUnmarshalJSON(m.Data, &c)

	s.mu.Lock()
	defer s.mu.Unlock()

	// Retrieve database.
	db := s.databases[c.Database]
	if s.databases[c.Database] == nil {
		return ErrDatabaseNotFound
	}

	// Validate retention policy.
	rp := db.policies[c.Policy]
	if rp == nil {
		return ErrRetentionPolicyNotFound
	}

	// If we can match to an existing shard group date range then just ignore request.
	if g := rp.shardGroupByTimestamp(c.Timestamp); g != nil {
		return nil
	}

	// If no shards match then create a new one.
	g := newShardGroup()
	g.StartTime = c.Timestamp.Truncate(rp.Duration).UTC()
	g.EndTime = g.StartTime.Add(rp.Duration).UTC()

	// Sort nodes so they're consistently assigned to the shards.
	nodes := make([]*DataNode, 0, len(s.dataNodes))
	for _, n := range s.dataNodes {
		nodes = append(nodes, n)
	}
	sort.Sort(dataNodes(nodes))

	// Require at least one replica but no more replicas than nodes.
	replicaN := int(rp.ReplicaN)
	if replicaN == 0 {
		replicaN = 1
	} else if replicaN > len(nodes) {
		replicaN = len(nodes)
	}

	// Determine shard count by node count divided by replication factor.
	// This will ensure nodes will get distributed across nodes evenly and
	// replicated the correct number of times.
	shardN := len(nodes) / replicaN

	// Create a shard based on the node count and replication factor.
	g.Shards = make([]*Shard, shardN)
	for i := range g.Shards {
		g.Shards[i] = newShard()
	}

	// Persist to metastore if a shard was created.
	if err = s.meta.mustUpdate(func(tx *metatx) error {
		// Generate an ID for the group.
		g.ID = tx.nextShardGroupID()

		// Generate an ID for each shard.
		for _, sh := range g.Shards {
			sh.ID = tx.nextShardID()
		}

		// Assign data nodes to shards via round robin.
		// Start from a repeatably "random" place in the node list.
		nodeIndex := int(m.Index % uint64(len(nodes)))
		for _, sh := range g.Shards {
			for i := 0; i < replicaN; i++ {
				node := nodes[nodeIndex%len(nodes)]
				sh.DataNodeIDs = append(sh.DataNodeIDs, node.ID)
				nodeIndex++
			}
		}

		// Retention policy has a new shard group, so update the policy.
		rp.shardGroups = append(rp.shardGroups, g)

		return tx.saveDatabase(db)
	}); err != nil {
		g.close()
		return
	}

	// Open shards assigned to this server.
	for _, sh := range g.Shards {
		// Ignore if this server is not assigned.
		if !sh.HasDataNodeID(s.id) {
			continue
		}

		// Open shard store. Panic if an error occurs and we can retry.
		if err := sh.open(s.shardPath(sh.ID)); err != nil {
			panic("unable to open shard: " + err.Error())
		}
	}

	// Add to lookups.
	for _, sh := range g.Shards {
		s.shards[sh.ID] = sh
	}

	// Subscribe to shard if it matches the server's index.
	// TODO: Move subscription outside of command processing.
	// TODO: Retry subscriptions on failure.
	for _, sh := range g.Shards {
		// Ignore if this server is not assigned.
		if !sh.HasDataNodeID(s.id) {
			continue
		}

		// Subscribe on the broker.
		if err := s.client.Subscribe(s.id, sh.ID); err != nil {
			log.Printf("unable to subscribe: replica=%d, topic=%d, err=%s", s.id, sh.ID, err)
		}
	}

	return
}

type createShardGroupIfNotExistsCommand struct {
	Database  string    `json:"database"`
	Policy    string    `json:"policy"`
	Timestamp time.Time `json:"timestamp"`
}

// DeleteShardGroup deletes the shard group identified by shardID.
func (s *Server) DeleteShardGroup(database, policy string, shardID uint64) error {
	c := &deleteShardGroupCommand{Database: database, Policy: policy, ID: shardID}
	_, err := s.broadcast(deleteShardGroupMessageType, c)
	return err
}

// applyDeleteShardGroup deletes shard data from disk and updates the metastore.
func (s *Server) applyDeleteShardGroup(m *messaging.Message) (err error) {
	var c deleteShardGroupCommand
	mustUnmarshalJSON(m.Data, &c)

	s.mu.Lock()
	defer s.mu.Unlock()

	// Retrieve database.
	db := s.databases[c.Database]
	if s.databases[c.Database] == nil {
		return ErrDatabaseNotFound
	}

	// Validate retention policy.
	rp := db.policies[c.Policy]
	if rp == nil {
		return ErrRetentionPolicyNotFound
	}

	// If shard group no longer exists, then ignore request. This can occur if multiple
	// data nodes triggered the deletion.
	g := rp.shardGroupByID(c.ID)
	if g == nil {
		return nil
	}

	for _, shard := range g.Shards {
		// Ignore shards not on this server.
		if !shard.HasDataNodeID(s.id) {
			continue
		}

		path := shard.store.Path()
		shard.close()
		if err := os.Remove(path); err != nil {
			// Log, but keep going. This can happen if shards were deleted, but the server exited
			// before it acknowledged the delete command.
			log.Printf("error deleting shard %s, group ID %d, policy %s: %s", path, g.ID, rp.Name, err.Error())
		}
	}

	// Remove from metastore.
	rp.removeShardGroupByID(c.ID)
	err = s.meta.mustUpdate(func(tx *metatx) error {
		return tx.saveDatabase(db)
	})
	return
}

type deleteShardGroupCommand struct {
	Database string `json:"database"`
	Policy   string `json:"policy"`
	ID       uint64 `json:"id"`
}

// User returns a user by username
// Returns nil if the user does not exist.
func (s *Server) User(name string) *User {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.users[name]
}

// Users returns a list of all users, sorted by name.
func (s *Server) Users() (a []*User) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, u := range s.users {
		a = append(a, u)
	}
	sort.Sort(users(a))
	return a
}

// UserCount returns the number of users.
func (s *Server) UserCount() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.users)
}

// AdminUserExists returns whether at least 1 admin-level user exists.
func (s *Server) AdminUserExists() bool {
	for _, u := range s.users {
		if u.Admin {
			return true
		}
	}
	return false
}

// Authenticate returns an authenticated user by username. If any error occurs,
// or the authentication credentials are invalid, an error is returned.
func (s *Server) Authenticate(username, password string) (*User, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	u := s.users[username]

	// If authorization is not enabled and user is nil, we are authorized
	if u == nil && !s.authenticationEnabled {
		return nil, nil
	}
	if u == nil {
		return nil, fmt.Errorf("invalid username or password")
	}
	err := u.Authenticate(password)
	if err != nil {
		return nil, fmt.Errorf("invalid username or password")
	}
	return u, nil
}

// CreateUser creates a user on the server.
func (s *Server) CreateUser(username, password string, admin bool) error {
	c := &createUserCommand{Username: username, Password: password, Admin: admin}
	_, err := s.broadcast(createUserMessageType, c)
	return err
}

func (s *Server) applyCreateUser(m *messaging.Message) (err error) {
	var c createUserCommand
	mustUnmarshalJSON(m.Data, &c)

	s.mu.Lock()
	defer s.mu.Unlock()

	// Validate user.
	if c.Username == "" {
		return ErrUsernameRequired
	} else if s.users[c.Username] != nil {
		return ErrUserExists
	}

	// Generate the hash of the password.
	hash, err := HashPassword(c.Password)
	if err != nil {
		return err
	}

	// Create the user.
	u := &User{
		Name:       c.Username,
		Hash:       string(hash),
		Privileges: make(map[string]influxql.Privilege),
		Admin:      c.Admin,
	}

	// Persist to metastore.
	err = s.meta.mustUpdate(func(tx *metatx) error {
		return tx.saveUser(u)
	})

	s.users[u.Name] = u
	return
}

type createUserCommand struct {
	Username string `json:"username"`
	Password string `json:"password"`
	Admin    bool   `json:"admin,omitempty"`
}

// UpdateUser updates an existing user on the server.
func (s *Server) UpdateUser(username, password string) error {
	c := &updateUserCommand{Username: username, Password: password}
	_, err := s.broadcast(updateUserMessageType, c)
	return err
}

func (s *Server) applyUpdateUser(m *messaging.Message) (err error) {
	var c updateUserCommand
	mustUnmarshalJSON(m.Data, &c)

	s.mu.Lock()
	defer s.mu.Unlock()

	// Validate command.
	u := s.users[c.Username]
	if u == nil {
		return ErrUserNotFound
	}

	// Update the user's password, if set.
	if c.Password != "" {
		hash, err := HashPassword(c.Password)
		if err != nil {
			return err
		}
		u.Hash = string(hash)
	}

	// Persist to metastore.
	return s.meta.mustUpdate(func(tx *metatx) error {
		return tx.saveUser(u)
	})
}

type updateUserCommand struct {
	Username string `json:"username"`
	Password string `json:"password,omitempty"`
}

// DeleteUser removes a user from the server.
func (s *Server) DeleteUser(username string) error {
	c := &deleteUserCommand{Username: username}
	_, err := s.broadcast(deleteUserMessageType, c)
	return err
}

func (s *Server) applyDeleteUser(m *messaging.Message) error {
	var c deleteUserCommand
	mustUnmarshalJSON(m.Data, &c)

	s.mu.Lock()
	defer s.mu.Unlock()

	// Validate user.
	if c.Username == "" {
		return ErrUsernameRequired
	} else if s.users[c.Username] == nil {
		return ErrUserNotFound
	}

	// Remove from metastore.
	s.meta.mustUpdate(func(tx *metatx) error {
		return tx.deleteUser(c.Username)
	})

	// Delete the user.
	delete(s.users, c.Username)
	return nil
}

type deleteUserCommand struct {
	Username string `json:"username"`
}

// SetPrivilege grants / revokes a privilege to a user.
func (s *Server) SetPrivilege(p influxql.Privilege, username string, dbname string) error {
	c := &setPrivilegeCommand{p, username, dbname}
	_, err := s.broadcast(setPrivilegeMessageType, c)
	return err
}

func (s *Server) applySetPrivilege(m *messaging.Message) error {
	var c setPrivilegeCommand
	mustUnmarshalJSON(m.Data, &c)

	s.mu.Lock()
	defer s.mu.Unlock()

	// Validate user.
	if c.Username == "" {
		return ErrUsernameRequired
	}

	u := s.users[c.Username]
	if u == nil {
		return ErrUserNotFound
	}

	// If dbname is empty, update user's Admin flag.
	if c.Database == "" && (c.Privilege == influxql.AllPrivileges || c.Privilege == influxql.NoPrivileges) {
		u.Admin = (c.Privilege == influxql.AllPrivileges)
	} else if c.Database != "" {
		// Update user's privilege for the database.
		u.Privileges[c.Database] = c.Privilege
	} else {
		return ErrInvalidGrantRevoke
	}

	// Persist to metastore.
	return s.meta.mustUpdate(func(tx *metatx) error {
		return tx.saveUser(u)
	})
}

type setPrivilegeCommand struct {
	Privilege influxql.Privilege `json:"privilege"`
	Username  string             `json:"username"`
	Database  string             `json:"database"`
}

// RetentionPolicy returns a retention policy by name.
// Returns an error if the database doesn't exist.
func (s *Server) RetentionPolicy(database, name string) (*RetentionPolicy, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	// Lookup database.
	db := s.databases[database]
	if db == nil {
		return nil, ErrDatabaseNotFound
	}

	return db.policies[name], nil
}

// DefaultRetentionPolicy returns the default retention policy for a database.
// Returns an error if the database doesn't exist.
func (s *Server) DefaultRetentionPolicy(database string) (*RetentionPolicy, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	// Lookup database.
	db := s.databases[database]
	if db == nil {
		return nil, ErrDatabaseNotFound
	}

	return db.policies[db.defaultRetentionPolicy], nil
}

// RetentionPolicies returns a list of retention polocies for a database.
// Returns an error if the database doesn't exist.
func (s *Server) RetentionPolicies(database string) ([]*RetentionPolicy, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	// Lookup database.
	db := s.databases[database]
	if db == nil {
		return nil, ErrDatabaseNotFound
	}

	// Retrieve the policies.
	a := make([]*RetentionPolicy, 0, len(db.policies))
	for _, p := range db.policies {
		a = append(a, p)
	}
	return a, nil
}

// CreateRetentionPolicy creates a retention policy for a database.
func (s *Server) CreateRetentionPolicy(database string, rp *RetentionPolicy) error {
	c := &createRetentionPolicyCommand{
		Database: database,
		Name:     rp.Name,
		Duration: rp.Duration,
		ReplicaN: rp.ReplicaN,
	}
	_, err := s.broadcast(createRetentionPolicyMessageType, c)
	return err
}

func (s *Server) applyCreateRetentionPolicy(m *messaging.Message) error {
	var c createRetentionPolicyCommand
	mustUnmarshalJSON(m.Data, &c)

	s.mu.Lock()
	defer s.mu.Unlock()

	// Retrieve the database.
	db := s.databases[c.Database]
	if s.databases[c.Database] == nil {
		return ErrDatabaseNotFound
	} else if c.Name == "" {
		return ErrRetentionPolicyNameRequired
	} else if db.policies[c.Name] != nil {
		return ErrRetentionPolicyExists
	}

	// Add policy to the database.
	db.policies[c.Name] = &RetentionPolicy{
		Name:     c.Name,
		Duration: c.Duration,
		ReplicaN: c.ReplicaN,
	}

	// Persist to metastore.
	s.meta.mustUpdate(func(tx *metatx) error {
		return tx.saveDatabase(db)
	})

	return nil
}

type createRetentionPolicyCommand struct {
	Database string        `json:"database"`
	Name     string        `json:"name"`
	Duration time.Duration `json:"duration"`
	ReplicaN uint32        `json:"replicaN"`
	SplitN   uint32        `json:"splitN"`
}

// RetentionPolicyUpdate represents retention policy fields that
// need to be updated.
type RetentionPolicyUpdate struct {
	Name     *string        `json:"name,omitempty"`
	Duration *time.Duration `json:"duration,omitempty"`
	ReplicaN *uint32        `json:"replicaN,omitempty"`
}

// UpdateRetentionPolicy updates an existing retention policy on a database.
func (s *Server) UpdateRetentionPolicy(database, name string, rpu *RetentionPolicyUpdate) error {
	c := &updateRetentionPolicyCommand{Database: database, Name: name, Policy: rpu}
	_, err := s.broadcast(updateRetentionPolicyMessageType, c)
	return err
}

type updateRetentionPolicyCommand struct {
	Database string                 `json:"database"`
	Name     string                 `json:"name"`
	Policy   *RetentionPolicyUpdate `json:"policy"`
}

func (s *Server) applyUpdateRetentionPolicy(m *messaging.Message) (err error) {
	var c updateRetentionPolicyCommand
	mustUnmarshalJSON(m.Data, &c)

	s.mu.Lock()
	defer s.mu.Unlock()

	// Validate command.
	db := s.databases[c.Database]
	if s.databases[c.Database] == nil {
		return ErrDatabaseNotFound
	} else if c.Name == "" {
		return ErrRetentionPolicyNameRequired
	}

	// Retrieve the policy.
	p := db.policies[c.Name]
	if db.policies[c.Name] == nil {
		return ErrRetentionPolicyNotFound
	}

	// Update the policy name.
	if c.Policy.Name != nil {
		delete(db.policies, p.Name)
		p.Name = *c.Policy.Name
		db.policies[p.Name] = p
	}

	// Update duration.
	if c.Policy.Duration != nil {
		p.Duration = *c.Policy.Duration
	}

	// Update replication factor.
	if c.Policy.ReplicaN != nil {
		p.ReplicaN = *c.Policy.ReplicaN
	}

	// Persist to metastore.
	err = s.meta.mustUpdate(func(tx *metatx) error {
		return tx.saveDatabase(db)
	})

	return
}

// DeleteRetentionPolicy removes a retention policy from a database.
func (s *Server) DeleteRetentionPolicy(database, name string) error {
	c := &deleteRetentionPolicyCommand{Database: database, Name: name}
	_, err := s.broadcast(deleteRetentionPolicyMessageType, c)
	return err
}

func (s *Server) applyDeleteRetentionPolicy(m *messaging.Message) (err error) {
	var c deleteRetentionPolicyCommand
	mustUnmarshalJSON(m.Data, &c)

	s.mu.Lock()
	defer s.mu.Unlock()

	// Retrieve the database.
	db := s.databases[c.Database]
	if s.databases[c.Database] == nil {
		return ErrDatabaseNotFound
	} else if c.Name == "" {
		return ErrRetentionPolicyNameRequired
	} else if db.policies[c.Name] == nil {
		return ErrRetentionPolicyNotFound
	}

	// Remove retention policy.
	delete(db.policies, c.Name)

	// Persist to metastore.
	err = s.meta.mustUpdate(func(tx *metatx) error {
		return tx.saveDatabase(db)
	})

	return
}

type deleteRetentionPolicyCommand struct {
	Database string `json:"database"`
	Name     string `json:"name"`
}

// SetDefaultRetentionPolicy sets the default policy to write data into and query from on a database.
func (s *Server) SetDefaultRetentionPolicy(database, name string) error {
	c := &setDefaultRetentionPolicyCommand{Database: database, Name: name}
	_, err := s.broadcast(setDefaultRetentionPolicyMessageType, c)
	return err
}

func (s *Server) applySetDefaultRetentionPolicy(m *messaging.Message) (err error) {
	var c setDefaultRetentionPolicyCommand
	mustUnmarshalJSON(m.Data, &c)

	s.mu.Lock()
	defer s.mu.Unlock()

	// Validate command.
	db := s.databases[c.Database]
	if s.databases[c.Database] == nil {
		return ErrDatabaseNotFound
	} else if db.policies[c.Name] == nil {
		return ErrRetentionPolicyNotFound
	}

	// Update default policy.
	db.defaultRetentionPolicy = c.Name

	// Persist to metastore.
	err = s.meta.mustUpdate(func(tx *metatx) error {
		return tx.saveDatabase(db)
	})

	return
}

type setDefaultRetentionPolicyCommand struct {
	Database string `json:"database"`
	Name     string `json:"name"`
}

type createMeasurementSubcommand struct {
	Name   string                       `json:"name"`
	Tags   map[string]map[string]string `json:"tags"`
	Fields map[string]influxql.DataType `json:"fields"`
}

type createMeasurementsIfNotExistsCommand struct {
	Database     string                                 `json:"database"`
	Measurements map[string]createMeasurementSubcommand `json:"measurements"`
}

func newCreateMeasurementsIfNotExistsCommand(database string) *createMeasurementsIfNotExistsCommand {
	return &createMeasurementsIfNotExistsCommand{
		Database:     database,
		Measurements: make(map[string]createMeasurementSubcommand),
	}
}

// addMeasurementIfNotExists adds the Measurement to the command, but only if not already present
// in the command.
func (c *createMeasurementsIfNotExistsCommand) addMeasurementIfNotExists(name string) error {
	_, ok := c.Measurements[name]
	if !ok {
		c.Measurements[name] = createMeasurementSubcommand{
			Name:   name,
			Tags:   make(map[string]map[string]string),
			Fields: make(map[string]influxql.DataType),
		}
	}
	return nil
}

// addSeriesIfNotExists adds the Series, identified by Measurement name and tag set, to
// the command, but only if not already present in the command.
func (c *createMeasurementsIfNotExistsCommand) addSeriesIfNotExists(measurement string, tags map[string]string) error {
	_, ok := c.Measurements[measurement]
	if !ok {
		return ErrMeasurementNotFound
	}

	tagset := string(marshalTags(tags))
	_, ok = c.Measurements[measurement].Tags[tagset]
	if ok {
		// Series already present in in subcommand, nothing to do.
		return nil
	}
	// Tag-set needs to added to subcommand.
	c.Measurements[measurement].Tags[tagset] = tags

	return nil
}

// addFieldIfNotExists adds the field to the command for the Measurement, but only if it is not already
// present. It will return an error if the field is present in the command, but is of a different type.
func (c *createMeasurementsIfNotExistsCommand) addFieldIfNotExists(measurement, name string, typ influxql.DataType) error {
	_, ok := c.Measurements[measurement]
	if !ok {
		return ErrMeasurementNotFound
	}

	t, ok := c.Measurements[measurement].Fields[name]
	if ok {
		if typ != t {
			return ErrFieldTypeConflict
		}
		// Field already present in subcommand with same type, nothing to do.
		return nil
	}
	// New field for this measurement so add it to the subcommand.
	c.Measurements[measurement].Fields[name] = typ
	return nil
}

// Point defines the values that will be written to the database
type Point struct {
	Name      string
	Tags      map[string]string
	Timestamp time.Time
	Values    map[string]interface{}
}

// WriteSeries writes series data to the database.
// Returns the messaging index the data was written to.
func (s *Server) WriteSeries(database, retentionPolicy string, points []Point) (uint64, error) {
	// If the retention policy is not set, use the default for this database.
	if retentionPolicy == "" {
		rp, err := s.DefaultRetentionPolicy(database)
		if err != nil {
			return 0, fmt.Errorf("failed to determine default retention policy: %s", err.Error())
		} else if rp == nil {
			return 0, ErrDefaultRetentionPolicyNotFound
		}
		retentionPolicy = rp.Name
	}

	// Ensure all required Series and Measurement Fields are created cluster-wide.
	if err := s.createMeasurementsIfNotExists(database, retentionPolicy, points); err != nil {
		return 0, err
	}

	// Ensure all the required shard groups exist. TODO: this should be done async.
	if err := s.createShardGroupsIfNotExists(database, retentionPolicy, points); err != nil {
		return 0, err
	}

	// Build writeRawSeriesMessageType publish commands.
	shardData := make(map[uint64][]byte, 0)
	for _, p := range points {
		// Local function makes lock management foolproof.
		measurement, series, err := func() (*Measurement, *Series, error) {
			s.mu.RLock()
			defer s.mu.RUnlock()
			db := s.databases[database]
			if db == nil {
				return nil, nil, fmt.Errorf("database not found %q", database)
			}
			if measurement, series := db.MeasurementAndSeries(p.Name, p.Tags); series != nil {
				return measurement, series, nil
			}
			panic("series not found")
		}()
		if err != nil {
			return 0, err
		}

		// Retrieve shard group.
		g, err := s.shardGroupByTimestamp(database, retentionPolicy, p.Timestamp)
		if err != nil {
			return 0, err
		}

		// Find appropriate shard within the shard group.
		sh := g.ShardBySeriesID(series.ID)

		// Get a field codec.
		s.mu.RLock()
		codec := NewFieldCodec(measurement)
		s.mu.RUnlock()
		if codec == nil {
			panic("field codec is nil")
		}

		// Convert string-key/values to encoded fields.
		encodedFields, err := codec.EncodeFields(p.Values)
		if err != nil {
			return 0, err
		}

		// Encode point header, followed by point data, and add to shard's batch.
		data := marshalPointHeader(series.ID, uint32(len(encodedFields)), p.Timestamp.UnixNano())
		data = append(data, encodedFields...)
		if shardData[sh.ID] == nil {
			shardData[sh.ID] = make([]byte, 0)
		}
		shardData[sh.ID] = append(shardData[sh.ID], data...)
	}

	// Write data for each shard to the Broker.
	var err error
	var maxIndex uint64
	for i, d := range shardData {
		index, err := s.client.Publish(&messaging.Message{
			Type:    writeRawSeriesMessageType,
			TopicID: i,
			Data:    d,
		})
		if err != nil {
			return maxIndex, err
		}
		if index > maxIndex {
			maxIndex = index
		}
	}

	return maxIndex, err
}

// applyWriteRawSeries writes raw series data to the database.
// Raw series data has already converted field names to ids so the
// representation is fast and compact.
func (s *Server) applyWriteRawSeries(m *messaging.Message) error {
	// Retrieve the shard.
	sh := s.Shard(m.TopicID)
	if sh == nil {
		return ErrShardNotFound
	}

	// TODO: Enable some way to specify if the data should be overwritten
	overwrite := true

	var i int
	for {
		seriesID, payloadLength, timestamp := unmarshalPointHeader(m.Data[:pointHeaderSize])
		data := m.Data[pointHeaderSize:payloadLength]

		// Add to lookup.
		s.addShardBySeriesID(sh, seriesID)

		// Write to shard.
		if err := sh.writeSeries(seriesID, timestamp, data, overwrite); err != nil {
			return err
		}

		if i > len(m.Data) {
			break
		}
	}

	return nil
}

func (s *Server) addShardBySeriesID(sh *Shard, seriesID uint32) {
	for _, other := range s.shardsBySeriesID[seriesID] {
		if other.ID == sh.ID {
			return
		}
	}
	s.shardsBySeriesID[seriesID] = append(s.shardsBySeriesID[seriesID], sh)
}

// createMeasurementsIfNotExists walks the "points" and ensures that all new Series are created, and all
// new Measurement fields have been created, across the cluster.
func (s *Server) createMeasurementsIfNotExists(database, retentionPolicy string, points []Point) error {
	c := newCreateMeasurementsIfNotExistsCommand(database)

	// Local function keeps lock management foolproof.
	func() error {
		s.mu.RLock()
		defer s.mu.RUnlock()

		db := s.databases[database]
		if db == nil {
			return fmt.Errorf("database not found %q", database)
		}

		for _, p := range points {
			measurement, series := db.MeasurementAndSeries(p.Name, p.Tags)

			if measurement == nil {
				// Measurement not in Metastore, add to command so it's created cluster-wide.
				c.addMeasurementIfNotExists(p.Name)
			}

			if series == nil {
				// Series does not exist in Metastore, add it so it's created cluster-wide.
				c.addSeriesIfNotExists(p.Name, p.Tags)
			}

			for k, v := range p.Values {
				if measurement != nil {
					if f := measurement.FieldByName(k); f != nil {
						// Field present in Metastore, make sure there is no type conflict.
						if f.Type != influxql.InspectDataType(v) {
							return fmt.Errorf(fmt.Sprintf("field \"%s\" is type %T, mapped as type %s", k, v, f.Type))
						} else {
							// Field does not exist in Metastore, add it so it's created cluster-wide.
							if err := c.addFieldIfNotExists(p.Name, k, influxql.InspectDataType(v)); err != nil {
								return err
							}
						}
					}
				} else {
					// Measurement does not exist in Metastore, so fields can't exist. Add each one unconditionally.
					if err := c.addFieldIfNotExists(p.Name, k, influxql.InspectDataType(v)); err != nil {
						return err
					}
				}
			}
		}

		return nil
	}()

	// Any broadcast actually required?
	if len(c.Measurements) > 0 {
		_, err := s.broadcast(createMeasurementsIfNotExistsMessageType, c)
		if err != nil {
			return err
		}
	}

	return nil
}

// applyCreateMeasurementsIfNotExists creates the Measurements, Series, and Fields in the Metastore.
func (s *Server) applyCreateMeasurementsIfNotExists(m *messaging.Message) error {
	var c createMeasurementsIfNotExistsCommand
	mustUnmarshalJSON(m.Data, &c)

	s.mu.Lock()
	defer s.mu.Unlock()
	// Validate command.
	db := s.databases[c.Database]
	if db == nil {
		return ErrDatabaseNotFound
	}

	// Process command within a transaction.
	if err := s.meta.mustUpdate(func(tx *metatx) error {
		for _, cm := range c.Measurements {
			// Create each series
			for _, t := range cm.Tags {
				_, ss := db.MeasurementAndSeries(cm.Name, t)

				// Ensure creation of Series is idempotent.
				if ss != nil {
					continue
				}

				series, err := tx.createSeries(db.name, cm.Name, t)
				if err != nil {
					return err
				}
				db.addSeriesToIndex(cm.Name, series)
			}

			// Create each new field.
			mm := db.measurements[cm.Name]
			if mm == nil {
				panic(fmt.Sprintf("Measurement %s does not exist", cm.Name))
			}
			for k, v := range cm.Fields {
				if err := mm.createFieldIfNotExists(k, v); err != nil {
					if err == ErrFieldOverflow {
						log.Printf("no more fields allowed: %s::%s", mm.Name, k)
						continue
					} else if err == ErrFieldTypeConflict {
						log.Printf("field type conflict: %s::%s", mm.Name, k)
						continue
					}
					return err
				}
				if err := tx.saveMeasurement(db.name, mm); err != nil {
					return fmt.Errorf("save measurement: %s", err)
				}
			}
			if err := tx.saveDatabase(db); err != nil {
				return fmt.Errorf("save database: %s", err)
			}
		}

		return nil
	}); err != nil {
		return err
	}

	return nil
}

// createShardGroupsIfNotExist walks the "points" and ensures that all required shards exist on the cluster.
func (s *Server) createShardGroupsIfNotExists(database, retentionPolicy string, points []Point) error {
	for _, p := range points {
		// Check if shard group exists first.
		g, err := s.shardGroupByTimestamp(database, retentionPolicy, p.Timestamp)
		if err != nil {
			return err
		} else if g != nil {
			continue
		}
		err = s.CreateShardGroupIfNotExists(database, retentionPolicy, p.Timestamp)
		if err != nil {
			return fmt.Errorf("create shard(%s/%s): %s", retentionPolicy, p.Timestamp.Format(time.RFC3339Nano), err)
		}
	}

	return nil
}

// ReadSeries reads a single point from a series in the database. It is used for debug and test only.
func (s *Server) ReadSeries(database, retentionPolicy, name string, tags map[string]string, timestamp time.Time) (map[string]interface{}, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	// Find database.
	db := s.databases[database]
	if db == nil {
		return nil, ErrDatabaseNotFound
	}

	// Find series.
	mm, series := db.MeasurementAndSeries(name, tags)
	if mm == nil {
		return nil, ErrMeasurementNotFound
	} else if series == nil {
		return nil, ErrSeriesNotFound
	}

	// If the retention policy is not specified, use the default for this database.
	if retentionPolicy == "" {
		retentionPolicy = db.defaultRetentionPolicy
	}

	// Retrieve retention policy.
	rp := db.policies[retentionPolicy]
	if rp == nil {
		return nil, ErrRetentionPolicyNotFound
	}

	// Retrieve shard group.
	g, err := s.shardGroupByTimestamp(database, retentionPolicy, timestamp)
	if err != nil {
		return nil, err
	} else if g == nil {
		return nil, nil
	}

	// TODO: Verify that server owns shard.

	// Find appropriate shard within the shard group.
	sh := g.Shards[int(series.ID)%len(g.Shards)]

	// Read raw encoded series data.
	data, err := sh.readSeries(series.ID, timestamp.UnixNano())
	if err != nil {
		return nil, err
	}

	// Decode into a raw value map.
	codec := NewFieldCodec(mm)
	rawValues := codec.DecodeFields(data)
	if rawValues == nil {
		return nil, nil
	}

	// Decode into a string-key value map.
	values := make(map[string]interface{}, len(rawValues))
	for fieldID, value := range rawValues {
		f := mm.Field(fieldID)
		if f == nil {
			continue
		}
		values[f.Name] = value
	}

	return values, nil
}

// ExecuteQuery executes an InfluxQL query against the server.
// Returns a resultset for each statement in the query.
// Stops on first execution error that occurs.
func (s *Server) ExecuteQuery(q *influxql.Query, database string, user *User) Results {
	// Authorize user to execute the query.
	if s.authenticationEnabled {
		if err := s.Authorize(user, q, database); err != nil {
			return Results{Err: err}
		}
	}

	// Build empty resultsets.
	results := Results{Results: make([]*Result, len(q.Statements))}

	// Execute each statement.
	for i, stmt := range q.Statements {
		// Set default database and policy on the statement.
		if err := s.NormalizeStatement(stmt, database); err != nil {
			results.Results[i] = &Result{Err: err}
			break
		}

		var res *Result
		switch stmt := stmt.(type) {
		case *influxql.SelectStatement:
			res = s.executeSelectStatement(stmt, database, user)
		case *influxql.CreateDatabaseStatement:
			res = s.executeCreateDatabaseStatement(stmt, user)
		case *influxql.DropDatabaseStatement:
			res = s.executeDropDatabaseStatement(stmt, user)
		case *influxql.ShowDatabasesStatement:
			res = s.executeShowDatabasesStatement(stmt, user)
		case *influxql.CreateUserStatement:
			res = s.executeCreateUserStatement(stmt, user)
		case *influxql.DropUserStatement:
			res = s.executeDropUserStatement(stmt, user)
		case *influxql.ShowUsersStatement:
			res = s.executeShowUsersStatement(stmt, user)
		case *influxql.DropSeriesStatement:
			continue
		case *influxql.ShowSeriesStatement:
			res = s.executeShowSeriesStatement(stmt, database, user)
		case *influxql.ShowMeasurementsStatement:
			res = s.executeShowMeasurementsStatement(stmt, database, user)
		case *influxql.ShowTagKeysStatement:
			res = s.executeShowTagKeysStatement(stmt, database, user)
		case *influxql.ShowTagValuesStatement:
			res = s.executeShowTagValuesStatement(stmt, database, user)
		case *influxql.ShowFieldKeysStatement:
			res = s.executeShowFieldKeysStatement(stmt, database, user)
		case *influxql.GrantStatement:
			res = s.executeGrantStatement(stmt, user)
		case *influxql.RevokeStatement:
			res = s.executeRevokeStatement(stmt, user)
		case *influxql.CreateRetentionPolicyStatement:
			res = s.executeCreateRetentionPolicyStatement(stmt, user)
		case *influxql.AlterRetentionPolicyStatement:
			res = s.executeAlterRetentionPolicyStatement(stmt, user)
		case *influxql.DropRetentionPolicyStatement:
			res = s.executeDropRetentionPolicyStatement(stmt, user)
		case *influxql.ShowRetentionPoliciesStatement:
			res = s.executeShowRetentionPoliciesStatement(stmt, user)
		case *influxql.CreateContinuousQueryStatement:
			res = s.executeCreateContinuousQueryStatement(stmt, user)
		case *influxql.DropContinuousQueryStatement:
			continue
		case *influxql.ShowContinuousQueriesStatement:
			res = s.executeShowContinuousQueriesStatement(stmt, database, user)
		default:
			panic(fmt.Sprintf("unsupported statement type: %T", stmt))
		}

		// If an error occurs then stop processing remaining statements.
		results.Results[i] = res
		if res.Err != nil {
			break
		}
	}

	// Fill any empty results after error.
	for i, res := range results.Results {
		if res == nil {
			results.Results[i] = &Result{Err: ErrNotExecuted}
		}
	}

	return results
}

// executeSelectStatement plans and executes a select statement against a database.
func (s *Server) executeSelectStatement(stmt *influxql.SelectStatement, database string, user *User) *Result {
	// Plan statement execution.
	e, err := s.planSelectStatement(stmt)
	if err != nil {
		return &Result{Err: err}
	}

	// Execute plan.
	ch, err := e.Execute()
	if err != nil {
		return &Result{Err: err}
	}

	// Read all rows from channel.
	res := &Result{Rows: make([]*influxql.Row, 0)}
	for row := range ch {
		res.Rows = append(res.Rows, row)
	}

	return res
}

// plans a selection statement under lock.
func (s *Server) planSelectStatement(stmt *influxql.SelectStatement) (*influxql.Executor, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	// If this is a wildcard statement, expand this to use the fields
	// This is temporary until we move other parts of the system further
	isWildcard := false
	for _, f := range stmt.Fields {
		if _, ok := f.Expr.(*influxql.Wildcard); ok {
			isWildcard = true
			break
		}
	}

	if len(stmt.Fields) != 1 && isWildcard {
		return nil, fmt.Errorf("unsupported query: %s.  currently only single wildcard is supported.", stmt.String())
	}

	if isWildcard {
		if measurement, ok := stmt.Source.(*influxql.Measurement); ok {
			segments, err := influxql.SplitIdent(measurement.Name)
			if err != nil {
				return nil, fmt.Errorf("unable to parse measurement %s", measurement.Name)
			}
			db, m := segments[0], segments[2]
			if s.databases[db].measurements[m] == nil {
				return nil, fmt.Errorf("measurement %s does not exist.", measurement.Name)
			}
			var fields influxql.Fields
			for _, f := range s.databases[db].measurements[m].Fields {
				fields = append(fields, &influxql.Field{Expr: &influxql.VarRef{Val: f.Name}})
			}
			stmt.Fields = fields
		}
	}

	// Plan query.
	p := influxql.NewPlanner(s)

	return p.Plan(stmt)
}

func (s *Server) executeCreateDatabaseStatement(q *influxql.CreateDatabaseStatement, user *User) *Result {
	return &Result{Err: s.CreateDatabase(q.Name)}
}

func (s *Server) executeDropDatabaseStatement(q *influxql.DropDatabaseStatement, user *User) *Result {
	return &Result{Err: s.DeleteDatabase(q.Name)}
}

func (s *Server) executeShowDatabasesStatement(q *influxql.ShowDatabasesStatement, user *User) *Result {
	row := &influxql.Row{Columns: []string{"name"}}
	for _, name := range s.Databases() {
		row.Values = append(row.Values, []interface{}{name})
	}
	return &Result{Rows: []*influxql.Row{row}}
}

func (s *Server) executeCreateUserStatement(q *influxql.CreateUserStatement, user *User) *Result {
	isAdmin := false
	if q.Privilege != nil {
		isAdmin = *q.Privilege == influxql.AllPrivileges
	}
	return &Result{Err: s.CreateUser(q.Name, q.Password, isAdmin)}
}

func (s *Server) executeDropUserStatement(q *influxql.DropUserStatement, user *User) *Result {
	return &Result{Err: s.DeleteUser(q.Name)}
}

func (s *Server) executeShowSeriesStatement(stmt *influxql.ShowSeriesStatement, database string, user *User) *Result {
	s.mu.RLock()
	defer s.mu.RUnlock()

	// Find the database.
	db := s.databases[database]
	if db == nil {
		return &Result{Err: ErrDatabaseNotFound}
	}

	// Get the list of measurements we're interested in.
	measurements, err := measurementsFromSourceOrDB(stmt.Source, db)
	if err != nil {
		return &Result{Err: err}
	}

	// // If OFFSET is past the end of the array, return empty results.
	// if stmt.Offset > len(measurements)-1 {
	// 	return &Result{}
	// }

	// Create result struct that will be populated and returned.
	result := &Result{
		Rows: make(influxql.Rows, 0, len(measurements)),
	}

	// Loop through measurements to build result. One result row / measurement.
	for _, m := range measurements {
		var ids seriesIDs

		if stmt.Condition != nil {
			// Get series IDs that match the WHERE clause.
			filters := map[uint32]influxql.Expr{}
			ids, _, _ = m.walkWhereForSeriesIds(stmt.Condition, filters)

			// If no series matched, then go to the next measurement.
			if len(ids) == 0 {
				continue
			}

			// TODO: check return of walkWhereForSeriesIds for fields
		} else {
			// No WHERE clause so get all series IDs for this measurement.
			ids = m.seriesIDs
		}

		// Make a new row for this measurement.
		r := &influxql.Row{
			Name:    m.Name,
			Columns: m.tagKeys(),
		}

		// Loop through series IDs getting matching tag sets.
		for _, id := range ids {
			if s, ok := m.seriesByID[id]; ok {
				values := make([]interface{}, 0, len(r.Columns))
				for _, column := range r.Columns {
					values = append(values, s.Tags[column])
				}

				// Add the tag values to the row.
				r.Values = append(r.Values, values)
			}
		}

		// Append the row to the result.
		result.Rows = append(result.Rows, r)
	}

	return result
}

func (s *Server) executeShowMeasurementsStatement(stmt *influxql.ShowMeasurementsStatement, database string, user *User) *Result {
	s.mu.RLock()
	defer s.mu.RUnlock()

	// Find the database.
	db := s.databases[database]
	if db == nil {
		return &Result{Err: ErrDatabaseNotFound}
	}

	// Get all measurements in sorted order.
	measurements := db.Measurements()
	sort.Sort(measurements)

	// If a WHERE clause was specified, filter the measurements.
	if stmt.Condition != nil {
		var err error
		measurements, err = db.measurementsByExpr(stmt.Condition)
		if err != nil {
			return &Result{Err: err}
		}
	}

	offset := stmt.Offset
	limit := stmt.Limit

	// If OFFSET is past the end of the array, return empty results.
	if offset > len(measurements)-1 {
		return &Result{}
	}

	// Calculate last index based on LIMIT.
	end := len(measurements)
	if limit > 0 && offset+limit < end {
		limit = offset + limit
	} else {
		limit = end
	}

	// Make a result row to hold all measurement names.
	row := &influxql.Row{
		Name:    "measurements",
		Columns: []string{"name"},
	}

	// Add one value to the row for each measurement name.
	for i := offset; i < limit; i++ {
		m := measurements[i]
		v := interface{}(m.Name)
		row.Values = append(row.Values, []interface{}{v})
	}

	// Make a result.
	result := &Result{
		Rows: influxql.Rows{row},
	}

	return result
}

func (s *Server) executeShowTagKeysStatement(stmt *influxql.ShowTagKeysStatement, database string, user *User) *Result {
	s.mu.RLock()
	defer s.mu.RUnlock()

	// Find the database.
	db := s.databases[database]
	if db == nil {
		return &Result{Err: ErrDatabaseNotFound}
	}

	// Get the list of measurements we're interested in.
	measurements, err := measurementsFromSourceOrDB(stmt.Source, db)
	if err != nil {
		return &Result{Err: err}
	}

	// Make result.
	result := &Result{
		Rows: make(influxql.Rows, 0, len(measurements)),
	}

	// Add one row per measurement to the result.
	for _, m := range measurements {
		// TODO: filter tag keys by stmt.Condition

		// Get the tag keys in sorted order.
		keys := m.tagKeys()

		// Convert keys to an [][]interface{}.
		values := make([][]interface{}, 0, len(m.seriesByTagKeyValue))
		for _, k := range keys {
			v := interface{}(k)
			values = append(values, []interface{}{v})
		}

		// Make a result row for the measurement.
		r := &influxql.Row{
			Name:    m.Name,
			Columns: []string{"tagKey"},
			Values:  values,
		}

		result.Rows = append(result.Rows, r)
	}

	// TODO: LIMIT & OFFSET

	return result
}

func (s *Server) executeShowTagValuesStatement(stmt *influxql.ShowTagValuesStatement, database string, user *User) *Result {
	s.mu.RLock()
	defer s.mu.RUnlock()

	// Find the database.
	db := s.databases[database]
	if db == nil {
		return &Result{Err: ErrDatabaseNotFound}
	}

	// Get the list of measurements we're interested in.
	measurements, err := measurementsFromSourceOrDB(stmt.Source, db)
	if err != nil {
		return &Result{Err: err}
	}

	// Make result.
	result := &Result{
		Rows: make(influxql.Rows, 0, len(measurements)),
	}

	for _, m := range measurements {
		var ids seriesIDs

		if stmt.Condition != nil {
			// Get series IDs that match the WHERE clause.
			filters := map[uint32]influxql.Expr{}
			ids, _, _ = m.walkWhereForSeriesIds(stmt.Condition, filters)

			// If no series matched, then go to the next measurement.
			if len(ids) == 0 {
				continue
			}

			// TODO: check return of walkWhereForSeriesIds for fields
		} else {
			// No WHERE clause so get all series IDs for this measurement.
			ids = m.seriesIDs
		}

		tagValues := m.tagValuesByKeyAndSeriesID(stmt.TagKeys, ids)

		r := &influxql.Row{
			Name:    m.Name,
			Columns: []string{"tagValue"},
		}

		vals := tagValues.list()
		sort.Strings(vals)

		for _, val := range vals {
			v := interface{}(val)
			r.Values = append(r.Values, []interface{}{v})
		}

		result.Rows = append(result.Rows, r)
	}

	return result
}

func (s *Server) executeShowContinuousQueriesStatement(stmt *influxql.ShowContinuousQueriesStatement, database string, user *User) *Result {
	rows := make([]*influxql.Row, 0)
	for _, name := range s.Databases() {
		row := &influxql.Row{Columns: []string{"name", "query"}, Name: name}
		for _, cq := range s.ContinuousQueries(name) {
			row.Values = append(row.Values, []interface{}{cq.cq.Name, cq.Query})
		}
		rows = append(rows, row)
	}
	return &Result{Rows: rows}
}

// filterMeasurementsByExpr filters a list of measurements by a tags expression.
func filterMeasurementsByExpr(measurements Measurements, expr influxql.Expr) (Measurements, error) {
	// Create a list to hold result measurements.
	filtered := make(Measurements, 0)

	// Iterate measurements adding the ones that match to the result.
	for _, m := range measurements {
		// Look up series IDs that match the tags expression.
		ids, err := m.seriesIDsAllOrByExpr(expr)
		if err != nil {
			return nil, err
		} else if len(ids) > 0 {
			filtered = append(filtered, m)
		}
	}
	sort.Sort(filtered)

	return filtered, nil
}

func (s *Server) executeShowFieldKeysStatement(stmt *influxql.ShowFieldKeysStatement, database string, user *User) *Result {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var err error

	// Find the database.
	db := s.databases[database]
	if db == nil {
		return &Result{Err: ErrDatabaseNotFound}
	}

	// Get the list of measurements we're interested in.
	measurements, err := measurementsFromSourceOrDB(stmt.Source, db)
	if err != nil {
		return &Result{Err: err}
	}

	// If the statement has a where clause, filter the measurements by it.
	if stmt.Condition != nil {
		if measurements, err = filterMeasurementsByExpr(measurements, stmt.Condition); err != nil {
			return &Result{Err: err}
		}
	}

	// Make result.
	result := &Result{
		Rows: make(influxql.Rows, 0, len(measurements)),
	}

	// Loop through measurements, adding a result row for each.
	for _, m := range measurements {
		// Create a new row.
		r := &influxql.Row{
			Name:    m.Name,
			Columns: []string{"fieldKey"},
		}

		// Get a list of field names from the measurement then sort them.
		names := make([]string, 0, len(m.Fields))
		for _, f := range m.Fields {
			names = append(names, f.Name)
		}
		sort.Strings(names)

		// Add the field names to the result row values.
		for _, n := range names {
			v := interface{}(n)
			r.Values = append(r.Values, []interface{}{v})
		}

		// Append the row to the result.
		result.Rows = append(result.Rows, r)
	}

	return result
}

func (s *Server) executeGrantStatement(stmt *influxql.GrantStatement, user *User) *Result {
	return &Result{Err: s.SetPrivilege(stmt.Privilege, stmt.User, stmt.On)}
}

func (s *Server) executeRevokeStatement(stmt *influxql.RevokeStatement, user *User) *Result {
	return &Result{Err: s.SetPrivilege(influxql.NoPrivileges, stmt.User, stmt.On)}
}

// measurementsFromSourceOrDB returns a list of measurements from the
// statement passed in or, if the statement is nil, a list of all
// measurement names from the database passed in.
func measurementsFromSourceOrDB(stmt influxql.Source, db *database) (Measurements, error) {
	var measurements Measurements
	if stmt != nil {
		// TODO: handle multiple measurement sources
		if m, ok := stmt.(*influxql.Measurement); ok {
			segments, err := influxql.SplitIdent(m.Name)
			if err != nil {
				return nil, err
			}
			name := m.Name
			if len(segments) == 3 {
				name = segments[2]
			}

			measurement := db.measurements[name]
			if measurement == nil {
				return nil, fmt.Errorf(`measurement "%s" not found`, name)
			}

			measurements = append(measurements, db.measurements[name])
		} else {
			return nil, errors.New("identifiers in FROM clause must be measurement names")
		}
	} else {
		// No measurements specified in FROM clause so get all measurements.
		measurements = db.Measurements()
	}
	sort.Sort(measurements)

	return measurements, nil
}

func (s *Server) executeShowUsersStatement(q *influxql.ShowUsersStatement, user *User) *Result {
	row := &influxql.Row{Columns: []string{"user", "admin"}}
	for _, user := range s.Users() {
		row.Values = append(row.Values, []interface{}{user.Name, user.Admin})
	}
	return &Result{Rows: []*influxql.Row{row}}
}

func (s *Server) executeCreateRetentionPolicyStatement(q *influxql.CreateRetentionPolicyStatement, user *User) *Result {
	rp := NewRetentionPolicy(q.Name)
	rp.Duration = q.Duration
	rp.ReplicaN = uint32(q.Replication)

	// Create new retention policy.
	err := s.CreateRetentionPolicy(q.Database, rp)
	if err != nil {
		return &Result{Err: err}
	}

	// If requested, set new policy as the default.
	if q.Default {
		err = s.SetDefaultRetentionPolicy(q.Database, q.Name)
	}

	return &Result{Err: err}
}

func (s *Server) executeAlterRetentionPolicyStatement(stmt *influxql.AlterRetentionPolicyStatement, user *User) *Result {
	rpu := &RetentionPolicyUpdate{
		Duration: stmt.Duration,
		ReplicaN: func() *uint32 { n := uint32(*stmt.Replication); return &n }(),
	}

	// Update the retention policy.
	err := s.UpdateRetentionPolicy(stmt.Database, stmt.Name, rpu)
	if err != nil {
		return &Result{Err: err}
	}

	// If requested, set as default retention policy.
	if stmt.Default {
		err = s.SetDefaultRetentionPolicy(stmt.Database, stmt.Name)
	}

	return &Result{Err: err}
}

func (s *Server) executeDropRetentionPolicyStatement(q *influxql.DropRetentionPolicyStatement, user *User) *Result {
	return &Result{Err: s.DeleteRetentionPolicy(q.Database, q.Name)}
}

func (s *Server) executeShowRetentionPoliciesStatement(q *influxql.ShowRetentionPoliciesStatement, user *User) *Result {
	a, err := s.RetentionPolicies(q.Database)
	if err != nil {
		return &Result{Err: err}
	}

	row := &influxql.Row{Columns: []string{"name", "duration", "replicaN"}}
	for _, rp := range a {
		row.Values = append(row.Values, []interface{}{rp.Name, rp.Duration.String(), rp.ReplicaN})
	}
	return &Result{Rows: []*influxql.Row{row}}
}

func (s *Server) executeCreateContinuousQueryStatement(q *influxql.CreateContinuousQueryStatement, user *User) *Result {
	return &Result{Err: s.CreateContinuousQuery(q)}
}

func (s *Server) CreateContinuousQuery(q *influxql.CreateContinuousQueryStatement) error {
	c := &createContinuousQueryCommand{Query: q.String()}
	_, err := s.broadcast(createContinuousQueryMessageType, c)
	return err
}

func (s *Server) ContinuousQueries(database string) []*ContinuousQuery {
	s.mu.RLock()
	defer s.mu.RUnlock()

	db := s.databases[database]
	if db == nil {
		return nil
	}

	return db.continuousQueries
}

// MeasurementNames returns a list of all measurements for the specified database.
func (s *Server) MeasurementNames(database string) []string {
	s.mu.RLock()
	defer s.mu.RUnlock()

	db := s.databases[database]
	if db == nil {
		return nil
	}

	return db.names
}

/*
func (s *Server) MeasurementSeriesIDs(database, measurement string) []uint32 {
	s.mu.RLock()
	defer s.mu.RUnlock()

	db := s.databases[database]
	if db == nil {
		return nil
	}

	return []uint32(db.SeriesIDs([]string{measurement}, nil))
}
*/

// measurement returns a measurement by database and name.
func (s *Server) measurement(database, name string) (*Measurement, error) {
	db := s.databases[database]
	if db == nil {
		return nil, ErrDatabaseNotFound
	}

	return db.measurements[name], nil
}

// Begin returns an unopened transaction associated with the server.
func (s *Server) Begin() (influxql.Tx, error) { return newTx(s), nil }

// NormalizeStatement adds a default database and policy to the measurements in statement.
func (s *Server) NormalizeStatement(stmt influxql.Statement, defaultDatabase string) (err error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	// Track prefixes for replacing field names.
	prefixes := make(map[string]string)

	// Qualify all measurements.
	influxql.WalkFunc(stmt, func(n influxql.Node) {
		if err != nil {
			return
		}
		switch n := n.(type) {
		case *influxql.Measurement:
			name, e := s.normalizeMeasurement(n.Name, defaultDatabase)
			if e != nil {
				err = e
				return
			}
			prefixes[n.Name] = name
			n.Name = name
		}
	})
	if err != nil {
		return err
	}

	// Replace all variable references that used measurement prefixes.
	influxql.WalkFunc(stmt, func(n influxql.Node) {
		switch n := n.(type) {
		case *influxql.VarRef:
			for k, v := range prefixes {
				if strings.HasPrefix(n.Val, k+".") {
					n.Val = v + "." + influxql.QuoteIdent([]string{n.Val[len(k)+1:]})
				}
			}
		}
	})

	return
}

// NormalizeMeasurement inserts the default database or policy into all measurement names.
func (s *Server) NormalizeMeasurement(name string, defaultDatabase string) (string, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.normalizeMeasurement(name, defaultDatabase)
}

func (s *Server) normalizeMeasurement(name string, defaultDatabase string) (string, error) {
	// Split name into segments.
	segments, err := influxql.SplitIdent(name)
	if err != nil {
		return "", fmt.Errorf("invalid measurement: %s", name)
	}

	// Normalize to 3 segments.
	switch len(segments) {
	case 1:
		segments = append([]string{"", ""}, segments...)
	case 2:
		segments = append([]string{""}, segments...)
	case 3:
		// nop
	default:
		return "", fmt.Errorf("invalid measurement: %s", name)
	}

	// Set database if unset.
	if segment := segments[0]; segment == `` {
		segments[0] = defaultDatabase
	}

	// Find database.
	db := s.databases[segments[0]]
	if db == nil {
		return "", fmt.Errorf("database not found: %s", segments[0])
	}

	// Set retention policy if unset.
	if segment := segments[1]; segment == `` {
		if db.defaultRetentionPolicy == "" {
			return "", fmt.Errorf("default retention policy not set for: %s", db.name)
		}
		segments[1] = db.defaultRetentionPolicy
	}

	// Check if retention policy exists.
	if _, ok := db.policies[segments[1]]; !ok {
		return "", fmt.Errorf("retention policy does not exist: %s.%s", segments[0], segments[1])
	}

	return influxql.QuoteIdent(segments), nil
}

// processor runs in a separate goroutine and processes all incoming broker messages.
func (s *Server) processor(client MessagingClient, done chan struct{}) {
	for {
		// Read incoming message.
		var m *messaging.Message
		var ok bool
		select {
		case <-done:
			return
		case m, ok = <-client.C():
			if !ok {
				return
			}
		}

		// Exit if closed.
		// TODO: Wrap this check in a lock with the apply itself.
		if !s.opened() {
			continue
		}

		// Process message.
		var err error
		switch m.Type {
		case writeRawSeriesMessageType:
			err = s.applyWriteRawSeries(m)
		case createDataNodeMessageType:
			err = s.applyCreateDataNode(m)
		case deleteDataNodeMessageType:
			err = s.applyDeleteDataNode(m)
		case createDatabaseMessageType:
			err = s.applyCreateDatabase(m)
		case deleteDatabaseMessageType:
			err = s.applyDeleteDatabase(m)
		case createUserMessageType:
			err = s.applyCreateUser(m)
		case updateUserMessageType:
			err = s.applyUpdateUser(m)
		case deleteUserMessageType:
			err = s.applyDeleteUser(m)
		case createRetentionPolicyMessageType:
			err = s.applyCreateRetentionPolicy(m)
		case updateRetentionPolicyMessageType:
			err = s.applyUpdateRetentionPolicy(m)
		case deleteRetentionPolicyMessageType:
			err = s.applyDeleteRetentionPolicy(m)
		case createShardGroupIfNotExistsMessageType:
			err = s.applyCreateShardGroupIfNotExists(m)
		case deleteShardGroupMessageType:
			err = s.applyDeleteShardGroup(m)
		case setDefaultRetentionPolicyMessageType:
			err = s.applySetDefaultRetentionPolicy(m)
		case createMeasurementsIfNotExistsMessageType:
			err = s.applyCreateMeasurementsIfNotExists(m)
		case setPrivilegeMessageType:
			err = s.applySetPrivilege(m)
		case createContinuousQueryMessageType:
			err = s.applyCreateContinuousQueryCommand(m)
		}

		// Sync high water mark and errors.
		s.mu.Lock()
		s.index = m.Index
		if err != nil {
			s.errors[m.Index] = err
		}
		s.mu.Unlock()
	}
}

// Result represents a resultset returned from a single statement.
type Result struct {
	Rows []*influxql.Row
	Err  error
}

// MarshalJSON encodes the result into JSON.
func (r *Result) MarshalJSON() ([]byte, error) {
	// Define a struct that outputs "error" as a string.
	var o struct {
		Rows []*influxql.Row `json:"rows,omitempty"`
		Err  string          `json:"error,omitempty"`
	}

	// Copy fields to output struct.
	o.Rows = r.Rows
	if r.Err != nil {
		o.Err = r.Err.Error()
	}

	return json.Marshal(&o)
}

// UnmarshalJSON decodes the data into the Result struct
func (r *Result) UnmarshalJSON(b []byte) error {
	var o struct {
		Rows []*influxql.Row `json:"rows,omitempty"`
		Err  string          `json:"error,omitempty"`
	}

	err := json.Unmarshal(b, &o)
	if err != nil {
		return err
	}
	r.Rows = o.Rows
	if o.Err != "" {
		r.Err = errors.New(o.Err)
	}
	return nil
}

// Results represents a list of statement results.
type Results struct {
	Results []*Result
	Err     error
}

// MarshalJSON encodes a Results struct into JSON.
func (r Results) MarshalJSON() ([]byte, error) {
	// Define a struct that outputs "error" as a string.
	var o struct {
		Results []*Result `json:"results,omitempty"`
		Err     string    `json:"error,omitempty"`
	}

	// Copy fields to output struct.
	o.Results = r.Results
	if r.Err != nil {
		o.Err = r.Err.Error()
	}

	return json.Marshal(&o)
}

// UnmarshalJSON decodes the data into the Results struct
func (r *Results) UnmarshalJSON(b []byte) error {
	var o struct {
		Results []*Result `json:"results,omitempty"`
		Err     string    `json:"error,omitempty"`
	}

	err := json.Unmarshal(b, &o)
	if err != nil {
		return err
	}
	r.Results = o.Results
	if o.Err != "" {
		r.Err = errors.New(o.Err)
	}
	return nil
}

// Error returns the first error from any statement.
// Returns nil if no errors occurred on any statements.
func (r *Results) Error() error {
	if r.Err != nil {
		return r.Err
	}
	for _, rr := range r.Results {
		if rr.Err != nil {
			return rr.Err
		}
	}
	return nil
}

// MessagingClient represents the client used to receive messages from brokers.
type MessagingClient interface {
	// Publishes a message to the broker.
	Publish(m *messaging.Message) (index uint64, err error)

	// Creates a new replica with a given ID on the broker.
	CreateReplica(replicaID uint64, connectURL *url.URL) error

	// Deletes an existing replica with a given ID from the broker.
	DeleteReplica(replicaID uint64) error

	// Creates a subscription for a replica to a topic.
	Subscribe(replicaID, topicID uint64) error

	// Removes a subscription from the replica for a topic.
	Unsubscribe(replicaID, topicID uint64) error

	// The streaming channel for all subscribed messages.
	C() <-chan *messaging.Message
}

// DataNode represents a data node in the cluster.
type DataNode struct {
	ID  uint64
	URL *url.URL
}

// newDataNode returns an instance of DataNode.
func newDataNode() *DataNode { return &DataNode{} }

type dataNodes []*DataNode

func (p dataNodes) Len() int           { return len(p) }
func (p dataNodes) Less(i, j int) bool { return p[i].ID < p[j].ID }
func (p dataNodes) Swap(i, j int)      { p[i], p[j] = p[j], p[i] }

// Authorize user u to execute query q on database.
// database can be "" for queries that do not require a database.
// If u is nil, this means authorization is disabled.
func (s *Server) Authorize(u *User, q *influxql.Query, database string) error {
	const authErrLogFmt = `unauthorized request | user: %q | query: %q | database %q\n`

	if u == nil {
		s.Logger.Printf(authErrLogFmt, "", q.String(), database)
		return ErrAuthorize{text: "no user provided"}
	}

	// Cluster admins can do anything.
	if u.Admin {
		return nil
	}

	// Check each statement in the query.
	for _, stmt := range q.Statements {
		// Get the privileges required to execute the statement.
		privs := stmt.RequiredPrivileges()

		// Make sure the user has each privilege required to execute
		// the statement.
		for _, p := range privs {
			// Use the db name specified by the statement or the db
			// name passed by the caller if one wasn't specified by
			// the statement.
			dbname := p.Name
			if dbname == "" {
				dbname = database
			}

			// Check if user has required privilege.
			if !u.Authorize(p.Privilege, dbname) {
				var msg string
				if dbname == "" {
					msg = "requires cluster admin"
				} else {
					msg = fmt.Sprintf("requires %s privilege on %s", p.Privilege.String(), dbname)
				}
				s.Logger.Printf(authErrLogFmt, u.Name, q.String(), database)
				return ErrAuthorize{
					text: fmt.Sprintf("%s not authorized to execute '%s'.  %s", u.Name, stmt.String(), msg),
				}
			}
		}
	}
	return nil
}

// BcryptCost is the cost associated with generating password with Bcrypt.
// This setting is lowered during testing to improve test suite performance.
var BcryptCost = 10

// User represents a user account on the system.
// It can be given read/write permissions to individual databases.
type User struct {
	Name       string                        `json:"name"`
	Hash       string                        `json:"hash"`
	Privileges map[string]influxql.Privilege `json:"privileges"` // db name to privilege
	Admin      bool                          `json:"admin,omitempty"`
}

// Authenticate returns nil if the password matches the user's password.
// Returns an error if the password was incorrect.
func (u *User) Authenticate(password string) error {
	return bcrypt.CompareHashAndPassword([]byte(u.Hash), []byte(password))
}

// Authorize returns true if the user is authorized and false if not.
func (u *User) Authorize(privilege influxql.Privilege, database string) bool {
	p, ok := u.Privileges[database]
	return (ok && p >= privilege) || (u.Admin)
}

// users represents a list of users, sortable by name.
type users []*User

func (p users) Len() int           { return len(p) }
func (p users) Less(i, j int) bool { return p[i].Name < p[j].Name }
func (p users) Swap(i, j int)      { p[i], p[j] = p[j], p[i] }

// Matcher can match either a Regex or plain string.
type Matcher struct {
	IsRegex bool
	Name    string
}

// Matches returns true of the name passed in matches this Matcher.
func (m *Matcher) Matches(name string) bool {
	if m.IsRegex {
		matches, _ := regexp.MatchString(m.Name, name)
		return matches
	}
	return m.Name == name
}

// HashPassword generates a cryptographically secure hash for password.
// Returns an error if the password is invalid or a hash cannot be generated.
func HashPassword(password string) ([]byte, error) {
	// The second arg is the cost of the hashing, higher is slower but makes
	// it harder to brute force, since it will be really slow and impractical
	return bcrypt.GenerateFromPassword([]byte(password), BcryptCost)
}

// ContinuousQuery represents a query that exists on the server and processes
// each incoming event.
type ContinuousQuery struct {
	Query string `json:"query"`

	mu              sync.Mutex
	cq              *influxql.CreateContinuousQueryStatement
	lastRun         time.Time
	intoDB          string
	intoRP          string
	intoMeasurement string
}

// NewContinuousQuery returns a ContinuousQuery object with a parsed influxql.CreateContinuousQueryStatement
func NewContinuousQuery(q string) (*ContinuousQuery, error) {
	stmt, err := influxql.NewParser(strings.NewReader(q)).ParseStatement()
	if err != nil {
		return nil, err
	}

	cq, ok := stmt.(*influxql.CreateContinuousQueryStatement)
	if !ok {
		return nil, errors.New("query isn't a continuous query")
	}

	cquery := &ContinuousQuery{
		Query: q,
		cq:    cq,
	}

	// set which database and retention policy, and measuremet a CQ is writing into
	a, err := influxql.SplitIdent(cq.Source.Target.Measurement)
	if err != nil {
		return nil, err
	}

	// set the default into database to the same as the from database
	cquery.intoDB = cq.Database

	if len(a) == 1 { // into only set the measurement name. keep default db and rp
		cquery.intoMeasurement = a[0]
	} else if len(a) == 2 { // into set the rp and the measurement
		cquery.intoRP = a[0]
		cquery.intoMeasurement = a[1]
	} else { // into set db, rp, and measurement
		cquery.intoDB = a[0]
		cquery.intoRP = a[1]
		cquery.intoMeasurement = a[2]
	}

	return cquery, nil
}

// applyCreateContinuousQueryCommand adds the continuous query to the database object and saves it to the metastore
func (s *Server) applyCreateContinuousQueryCommand(m *messaging.Message) error {
	var c createContinuousQueryCommand
	mustUnmarshalJSON(m.Data, &c)

	cq, err := NewContinuousQuery(c.Query)
	if err != nil {
		return err
	}

	// normalize the select statement in the CQ so that it has the database and retention policy inserted
	if err := s.NormalizeStatement(cq.cq.Source, cq.cq.Database); err != nil {
		return err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	// ensure the into database exists
	if s.databases[cq.intoDB] == nil {
		return ErrDatabaseNotFound
	}

	// Retrieve the database.
	db := s.databases[cq.cq.Database]
	if db == nil {
		return ErrDatabaseNotFound
	} else if db.continuousQueryByName(cq.cq.Name) != nil {
		return ErrContinuousQueryExists
	}

	// Add cq to the database.
	db.continuousQueries = append(db.continuousQueries, cq)

	// Persist to metastore.
	s.meta.mustUpdate(func(tx *metatx) error {
		return tx.saveDatabase(db)
	})

	return nil
}

// RunContinuousQueries will run any continuous queries that are due to run and write the
// results back into the database
func (s *Server) RunContinuousQueries() error {
	s.mu.RLock()
	defer s.mu.RUnlock()

	for _, d := range s.databases {
		for _, c := range d.continuousQueries {
			if s.shouldRunContinuousQuery(c) {
				// set the into retention policy based on what is now the default
				if c.intoRP == "" {
					c.intoRP = d.defaultRetentionPolicy
				}
				go func(cq *ContinuousQuery) {
					s.runContinuousQuery(c)
				}(c)
			}
		}
	}

	return nil
}

// shouldRunContinuousQuery returns true if the CQ should be schedule to run. It will use the
// lastRunTime of the CQ and the rules for when to run set through the config to determine
// if this CQ should be run
func (s *Server) shouldRunContinuousQuery(cq *ContinuousQuery) bool {
	// if it's not aggregated we don't run it
	if !cq.cq.Source.Aggregated() {
		return false
	}

	// since it's aggregated we need to figure how often it should be run
	interval, err := cq.cq.Source.GroupByInterval()
	if err != nil {
		return false
	}

	// determine how often we should run this continuous query.
	// group by time / the number of times to compute
	computeEvery := time.Duration(interval.Nanoseconds()/int64(s.ComputeRunsPerInterval)) * time.Nanosecond
	// make sure we're running no more frequently than the setting in the config
	if computeEvery < s.ComputeNoMoreThan {
		computeEvery = s.ComputeNoMoreThan
	}

	// if we've passed the amount of time since the last run, do it up
	if cq.lastRun.Add(computeEvery).UnixNano() <= time.Now().UnixNano() {
		return true
	}

	return false
}

// runContinuousQuery will execute a continuous query
// TODO: make this fan out to the cluster instead of running all the queries on this single data node
func (s *Server) runContinuousQuery(cq *ContinuousQuery) {
	cq.mu.Lock()
	defer cq.mu.Unlock()

	now := time.Now()
	cq.lastRun = now

	interval, err := cq.cq.Source.GroupByInterval()
	if err != nil || interval == 0 {
		return
	}

	startTime := now.Round(interval)
	if startTime.UnixNano() > now.UnixNano() {
		startTime = startTime.Add(-interval)
	}

	if err := cq.cq.Source.SetTimeRange(startTime, startTime.Add(interval)); err != nil {
		log.Printf("cq error setting time range: %s\n", err.Error())
	}

	if err := s.runContinuousQueryAndWriteResult(cq); err != nil {
		log.Printf("cq error: %s. running: %s\n", err.Error(), cq.cq.String())
	}

	for i := 0; i < s.RecomputePreviousN; i++ {
		// if we're already more time past the previous window than we're going to look back, stop
		if now.Sub(startTime) > s.RecomputeNoOlderThan {
			return
		}
		newStartTime := startTime.Add(-interval)

		if err := cq.cq.Source.SetTimeRange(newStartTime, startTime); err != nil {
			log.Printf("cq error setting time range: %s\n", err.Error())
		}

		if err := s.runContinuousQueryAndWriteResult(cq); err != nil {
			log.Printf("cq error: %s. running: %s\n", err.Error(), cq.cq.String())
		}

		startTime = newStartTime
	}
}

// runContinuousQueryAndWriteResult will run the query against the cluster and write the results back in
func (s *Server) runContinuousQueryAndWriteResult(cq *ContinuousQuery) error {
	e, err := s.planSelectStatement(cq.cq.Source)

	if err != nil {
		return err
	}

	// Execute plan.
	ch, err := e.Execute()
	if err != nil {
		return err
	}

	// Read all rows from channel and write them in
	for row := range ch {
		points, err := s.convertRowToPoints(cq.intoMeasurement, row)
		if err != nil {
			log.Println(err)
			continue
		}

		if len(points) > 0 {
			_, err = s.WriteSeries(cq.intoDB, cq.intoRP, points)
			if err != nil {
				log.Printf("[cq] err: %s", err)
			}
		}
	}

	return nil
}

// convertRowToPoints will convert a query result Row into Points that can be written back in.
// Used for continuous and INTO queries
func (s *Server) convertRowToPoints(measurementName string, row *influxql.Row) ([]Point, error) {
	// figure out which parts of the result are the time and which are the fields
	timeIndex := -1
	fieldIndexes := make(map[string]int)
	for i, c := range row.Columns {
		if c == "time" {
			timeIndex = i
		} else {
			fieldIndexes[c] = i
		}
	}

	if timeIndex == -1 {
		return nil, errors.New("cq error finding time index in result")
	}

	points := make([]Point, 0, len(row.Values))
	for _, v := range row.Values {
		vals := make(map[string]interface{})
		for fieldName, fieldIndex := range fieldIndexes {
			vals[fieldName] = v[fieldIndex]
		}

		p := &Point{
			Name:      measurementName,
			Tags:      row.Tags,
			Timestamp: v[timeIndex].(time.Time),
			Values:    vals,
		}

		points = append(points, *p)
	}

	return points, nil
}

// createContinuousQueryCommand is the raft command for creating a continuous query on a database
type createContinuousQueryCommand struct {
	Query string `json:"query"`
}

// copyURL returns a copy of the the URL.
func copyURL(u *url.URL) *url.URL {
	other := &url.URL{}
	*other = *u
	return other
}
