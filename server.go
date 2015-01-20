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

	"code.google.com/p/go.crypto/bcrypt"
	"github.com/influxdb/influxdb/influxql"
	"github.com/influxdb/influxdb/messaging"
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
	DefaultShardRetention = time.Duration(0)
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

	// Series messages
	createSeriesIfNotExistsMessageType = messaging.MessageType(0x50)

	// Write series data messages (per-topic)
	writeRawSeriesMessageType = messaging.MessageType(0x80)
	writeSeriesMessageType    = messaging.MessageType(0x81)
)

// Server represents a collection of metadata and raw metric data.
type Server struct {
	mu   sync.RWMutex
	id   uint64
	path string
	done chan struct{} // goroutine close notification

	client MessagingClient  // broker client
	index  uint64           // highest broadcast index seen
	errors map[uint64]error // message errors

	meta *metastore // metadata store

	dataNodes map[uint64]*DataNode // data nodes by id
	databases map[string]*database // databases by name
	shards    map[uint64]*Shard    // shards by id
	users     map[string]*User     // user by name
}

// NewServer returns a new instance of Server.
func NewServer() *Server {
	return &Server{
		meta:      &metastore{},
		errors:    make(map[uint64]error),
		dataNodes: make(map[uint64]*DataNode),
		databases: make(map[string]*database),
		shards:    make(map[uint64]*Shard),
		users:     make(map[string]*User),
	}
}

// ID returns the data node id for the server.
// Returns zero if the server is closed or the server has not joined a cluster.
func (s *Server) ID() uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.id
}

// Path returns the path used when opening the server.
// Returns an empty string when the server is closed.
func (s *Server) Path() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.path
}

// shardPath returns the path for a shard.
func (s *Server) shardPath(id uint64) string {
	if s.path == "" {
		return ""
	}
	return filepath.Join(s.path, "shards", strconv.FormatUint(id, 10))
}

// Open initializes the server from a given path.
func (s *Server) Open(path string) error {
	// Ensure the server isn't already open and there's a path provided.
	if s.opened() {
		return ErrServerOpen
	} else if path == "" {
		return ErrPathRequired
	}

	// Create required directories.
	if err := os.MkdirAll(path, 0700); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Join(path, "shards"), 0700); err != nil {
		return err
	}

	// Open metadata store.
	if err := s.meta.open(filepath.Join(path, "meta")); err != nil {
		return fmt.Errorf("meta: %s", err)
	}

	// Load state from metastore.
	if err := s.load(); err != nil {
		return fmt.Errorf("load: %s", err)
	}

	// Set the server path.
	s.path = path

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

	// Remove path.
	s.path = ""

	// Close message processing.
	s.setClient(nil)

	// Close metastore.
	_ = s.meta.close()

	return nil
}

// load reads the state of the server from the metastore.
func (s *Server) load() error {
	return s.meta.view(func(tx *metatx) error {
		// Read server id.
		s.id = tx.id()

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

		// Load users.
		s.users = make(map[string]*User)
		for _, u := range tx.users() {
			s.users[u.Name] = u
		}

		return nil
	})
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
	resp, err = http.Post(joinURL.String(), "application/octet-stream", &buf)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	// Update the ID on the metastore.
	if err := s.meta.mustUpdate(func(tx *metatx) error {
		return tx.setID(n.ID)
	}); err != nil {
		return err
	}

	// Set the ID on the server.
	s.id = n.ID

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

// CreateShardGroupIfNotExist creates the shard group for a retention policy for the interval a timestamp falls into.
func (s *Server) CreateShardGroupIfNotExists(database, policy string, timestamp time.Time) error {
	c := &createShardGroupIfNotExistsCommand{Database: database, Policy: policy, Timestamp: timestamp}
	_, err := s.broadcast(createShardGroupIfNotExistsMessageType, c)
	return err
}

// createShardIfNotExists returns the shard group for a database, policy, and timestamp.
// If the group doesn't exist then one will be created automatically.
func (s *Server) createShardGroupIfNotExists(database, policy string, timestamp time.Time) (*ShardGroup, error) {
	// Check if shard group exists first.
	g, err := s.shardGroupByTimestamp(database, policy, timestamp)
	if err != nil {
		return nil, err
	} else if g != nil {
		return g, nil
	}

	// If the shard doesn't exist then create it.
	if err := s.CreateShardGroupIfNotExists(database, policy, timestamp); err != nil {
		return nil, err
	}

	// Lookup the shard again.
	return s.shardGroupByTimestamp(database, policy, timestamp)
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
	rp.shardGroups = append(rp.shardGroups, g)

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
	if u == nil {
		return nil, fmt.Errorf("user not found")
	}
	err := u.Authenticate(password)
	if err != nil {
		return nil, fmt.Errorf("invalid credentials")
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
		Name:  c.Username,
		Hash:  string(hash),
		Admin: c.Admin,
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
	s.mu.Lock()
	defer s.mu.Unlock()

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

// UpdateRetentionPolicy updates an existing retention policy on a database.
func (s *Server) UpdateRetentionPolicy(database, name string, rp *RetentionPolicy) error {
	c := &updateRetentionPolicyCommand{Database: database, Name: name, NewName: rp.Name}
	_, err := s.broadcast(updateRetentionPolicyMessageType, c)
	return err
}

type updateRetentionPolicyCommand struct {
	Database string `json:"database"`
	Name     string `json:"name"`
	NewName  string `json:"newName"`
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

	// Update the policy name, if not blank.
	if c.NewName != c.Name && c.NewName != "" {
		delete(db.policies, p.Name)
		p.Name = c.NewName
		db.policies[p.Name] = p
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

func (s *Server) applyCreateSeriesIfNotExists(m *messaging.Message) error {
	var c createSeriesIfNotExistsCommand
	mustUnmarshalJSON(m.Data, &c)

	s.mu.Lock()
	defer s.mu.Unlock()

	// Validate command.
	db := s.databases[c.Database]
	if db == nil {
		return ErrDatabaseNotFound
	}

	if _, series := db.MeasurementAndSeries(c.Name, c.Tags); series != nil {
		return nil
	}

	// save to the metastore and add it to the in memory index
	var series *Series
	if err := s.meta.mustUpdate(func(tx *metatx) error {
		var err error
		series, err = tx.createSeries(db.name, c.Name, c.Tags)
		return err
	}); err != nil {
		return err
	}

	db.addSeriesToIndex(c.Name, series)

	return nil
}

type createSeriesIfNotExistsCommand struct {
	Database string            `json:"database"`
	Name     string            `json:"name"`
	Tags     map[string]string `json:"tags"`
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
	// TODO corylanou: implement batch writing
	if len(points) != 1 {
		return 0, errors.New("batching WriteSeries has not been implemented yet")
	}
	name, tags, timestamp, values := points[0].Name, points[0].Tags, points[0].Timestamp, points[0].Values

	// Find the id for the series and tagset
	seriesID, err := s.createSeriesIfNotExists(database, name, tags)
	if err != nil {
		return 0, err
	}

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

	// Retrieve measurement.
	m, err := s.measurement(database, name)
	if err != nil {
		return 0, err
	} else if m == nil {
		return 0, ErrMeasurementNotFound
	}

	// Retrieve shard group.
	g, err := s.createShardGroupIfNotExists(database, retentionPolicy, timestamp)
	if err != nil {
		return 0, fmt.Errorf("create shard(%s/%s): %s", retentionPolicy, timestamp.Format(time.RFC3339Nano), err)
	}

	// Find appropriate shard within the shard group.
	sh := g.ShardBySeriesID(seriesID)

	// Ignore requests that have no values.
	if len(values) == 0 {
		return 0, nil
	}

	// Convert string-key/values to fieldID-key/values.
	// If not all fields can be converted then send as a non-raw write series.
	rawValues := m.mapValues(values)
	if rawValues == nil {
		// Encode the command.
		data := mustMarshalJSON(&writeSeriesCommand{
			Database:    database,
			Measurement: name,
			SeriesID:    seriesID,
			Timestamp:   timestamp.UnixNano(),
			Values:      values,
		})

		// Publish "write series" message on shard's topic to broker.
		return s.client.Publish(&messaging.Message{
			Type:    writeSeriesMessageType,
			TopicID: sh.ID,
			Data:    data,
		})
	}

	// If we can successfully encode the string keys to raw field ids then
	// we can send a raw write series message which is much smaller and faster.

	// Encode point header.
	data := marshalPointHeader(seriesID, timestamp.UnixNano())
	data = append(data, marshalValues(rawValues)...)

	// Publish "raw write series" message on shard's topic to broker.
	return s.client.Publish(&messaging.Message{
		Type:    writeRawSeriesMessageType,
		TopicID: sh.ID,
		Data:    data,
	})
}

type writeSeriesCommand struct {
	Database    string                 `json:"database"`
	Measurement string                 `json:"measurement"`
	SeriesID    uint32                 `json:"seriesID"`
	Timestamp   int64                  `json:"timestamp"`
	Values      map[string]interface{} `json:"values"`
}

// applyWriteSeries writes "non-raw" series data to the database.
// Non-raw data occurs when fields have not been created yet so the field
// names cannot be converted to field ids.
func (s *Server) applyWriteSeries(m *messaging.Message) error {
	var c writeSeriesCommand
	mustUnmarshalJSON(m.Data, &c)

	s.mu.Lock()
	defer s.mu.Unlock()

	// Retrieve the shard.
	sh := s.shards[m.TopicID]
	if sh == nil {
		return ErrShardNotFound
	}

	// Retrieve the database.
	db := s.databases[c.Database]
	if db == nil {
		return ErrDatabaseNotFound
	}

	// Retrieve the measurement.
	mm := db.measurements[c.Measurement]
	if mm == nil {
		return ErrMeasurementNotFound
	}

	// Encode value map and create fields as needed.
	rawValues := make(map[uint8]interface{}, len(c.Values))
	for k, v := range c.Values {
		// TODO: Support non-float types.

		// Find or create fields.
		// If too many fields are on the measurement then log the issue.
		// If any other error occurs then exit.
		f, err := mm.createFieldIfNotExists(k, influxql.Number)
		if err == ErrFieldOverflow {
			log.Printf("no more fields allowed: %s::%s", mm.Name, k)
			continue
		} else if err != nil {
			return err
		}
		rawValues[f.ID] = v
	}

	// Update metastore.
	if err := s.meta.mustUpdate(func(tx *metatx) error {
		return tx.saveDatabase(db)
	}); err != nil {
		return err
	}

	// Encode the values into a binary format.
	data := marshalValues(rawValues)

	// TODO: Enable some way to specify if the data should be overwritten
	overwrite := true

	// Write to shard.
	return sh.writeSeries(c.SeriesID, c.Timestamp, data, overwrite)
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

	// Extract the series id and timestamp from the header.
	// Everything after the header is the marshalled value.
	seriesID, timestamp := unmarshalPointHeader(m.Data[:pointHeaderSize])
	data := m.Data[pointHeaderSize:]

	// TODO: Enable some way to specify if the data should be overwritten
	overwrite := true

	// Write to shard.
	return sh.writeSeries(seriesID, timestamp, data, overwrite)
}

func (s *Server) createSeriesIfNotExists(database, name string, tags map[string]string) (uint32, error) {
	// Try to find series locally first.
	s.mu.RLock()
	idx := s.databases[database]
	if idx == nil {
		return 0, fmt.Errorf("database not found %q", database)
	}
	if _, series := idx.MeasurementAndSeries(name, tags); series != nil {
		s.mu.RUnlock()
		return series.ID, nil
	}
	// release the read lock so the broadcast can actually go through and acquire the write lock
	s.mu.RUnlock()

	// If it doesn't exist then create a message and broadcast.
	c := &createSeriesIfNotExistsCommand{Database: database, Name: name, Tags: tags}
	_, err := s.broadcast(createSeriesIfNotExistsMessageType, c)
	if err != nil {
		return 0, err
	}

	// Lookup series again.
	_, series := idx.MeasurementAndSeries(name, tags)
	if series == nil {
		return 0, ErrSeriesNotFound
	}
	return series.ID, nil
}

// ReadSeries reads a single point from a series in the database.
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
	rawValues := unmarshalValues(data)
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
	// Build empty resultsets.
	results := make(Results, len(q.Statements))

	// Execute each statement.
	for i, stmt := range q.Statements {
		var res *Result
		switch stmt := stmt.(type) {
		case *influxql.SelectStatement:
			res = s.executeSelectStatement(stmt, database, user)
		case *influxql.CreateDatabaseStatement:
			res = s.executeCreateDatabaseStatement(stmt, user)
		case *influxql.DropDatabaseStatement:
			res = s.executeDropDatabaseStatement(stmt, user)
		case *influxql.ListDatabasesStatement:
			res = s.executeListDatabasesStatement(stmt, user)
		case *influxql.CreateUserStatement:
			res = s.executeCreateUserStatement(stmt, user)
		case *influxql.DropUserStatement:
			res = s.executeDropUserStatement(stmt, user)
		case *influxql.DropSeriesStatement:
			continue
		case *influxql.ListSeriesStatement:
			continue
		case *influxql.ListMeasurementsStatement:
			continue
		case *influxql.ListTagKeysStatement:
			continue
		case *influxql.ListTagValuesStatement:
			continue
		case *influxql.ListFieldKeysStatement:
			continue
		case *influxql.ListFieldValuesStatement:
			continue
		case *influxql.GrantStatement:
			continue
		case *influxql.RevokeStatement:
			continue
		case *influxql.CreateRetentionPolicyStatement:
			res = s.executeCreateRetentionPolicyStatement(stmt, user)
		case *influxql.AlterRetentionPolicyStatement:
			res = s.executeAlterRetentionPolicyStatement(stmt, user)
		case *influxql.DropRetentionPolicyStatement:
			res = s.executeDropRetentionPolicyStatement(stmt, user)
		case *influxql.ListRetentionPoliciesStatement:
			res = s.executeListRetentionPoliciesStatement(stmt, user)
		case *influxql.CreateContinuousQueryStatement:
			continue
		case *influxql.DropContinuousQueryStatement:
			continue
		case *influxql.ListContinuousQueriesStatement:
			continue
		default:
			panic(fmt.Sprintf("unsupported statement type: %T", stmt))
		}

		// If an error occurs then stop processing remaining statements.
		results[i] = res
		if res.Err != nil {
			break
		}
	}

	// Fill any empty results after error.
	for i, res := range results {
		if res == nil {
			results[i] = &Result{Err: ErrNotExecuted}
		}
	}

	return results
}

// executeSelectStatement plans and executes a select statement against a database.
func (s *Server) executeSelectStatement(stmt *influxql.SelectStatement, database string, user *User) *Result {
	// Plan statement execution.
	e, err := s.planSelectStatement(stmt, database)
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
func (s *Server) planSelectStatement(stmt *influxql.SelectStatement, database string) (*influxql.Executor, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	// Find database.
	db := s.databases[database]
	if db == nil {
		return nil, ErrDatabaseNotFound
	}

	// Plan query.
	p := influxql.NewPlanner(&dbi{server: s, db: db})
	return p.Plan(stmt)
}

func (s *Server) executeCreateDatabaseStatement(q *influxql.CreateDatabaseStatement, user *User) *Result {
	return &Result{Err: s.CreateDatabase(q.Name)}
}

func (s *Server) executeDropDatabaseStatement(q *influxql.DropDatabaseStatement, user *User) *Result {
	return &Result{Err: s.DeleteDatabase(q.Name)}
}

func (s *Server) executeListDatabasesStatement(q *influxql.ListDatabasesStatement, user *User) *Result {
	row := &influxql.Row{Columns: []string{"Name"}}
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

func (s *Server) executeCreateRetentionPolicyStatement(q *influxql.CreateRetentionPolicyStatement, user *User) *Result {
	rp := NewRetentionPolicy(q.Name)
	rp.Duration = q.Duration
	rp.ReplicaN = uint32(q.Replication)
	return &Result{Err: s.CreateRetentionPolicy(q.Database, rp)}
}

func (s *Server) executeAlterRetentionPolicyStatement(q *influxql.AlterRetentionPolicyStatement, user *User) *Result {
	rp := NewRetentionPolicy(q.Name)
	if q.Duration != nil {
		rp.Duration = *q.Duration
	}
	if q.Replication != nil {
		rp.ReplicaN = uint32(*q.Replication)
	}
	return &Result{Err: s.UpdateRetentionPolicy(q.Database, q.Name, rp)}
}

func (s *Server) executeDropRetentionPolicyStatement(q *influxql.DropRetentionPolicyStatement, user *User) *Result {
	return &Result{Err: s.DeleteRetentionPolicy(q.Database, q.Name)}
}

func (s *Server) executeListRetentionPoliciesStatement(q *influxql.ListRetentionPoliciesStatement, user *User) *Result {
	a, err := s.RetentionPolicies(q.Database)
	if err != nil {
		return &Result{Err: err}
	}

	row := &influxql.Row{Columns: []string{"Name"}}
	for _, rp := range a {
		row.Values = append(row.Values, []interface{}{rp.Name})
	}
	return &Result{Rows: []*influxql.Row{row}}
}

func (s *Server) MeasurementNames(database string) []string {
	s.mu.RLock()
	defer s.mu.RUnlock()

	db := s.databases[database]
	if db == nil {
		return nil
	}

	return db.names
}

func (s *Server) MeasurementSeriesIDs(database, measurement string) SeriesIDs {
	s.mu.RLock()
	defer s.mu.RUnlock()

	db := s.databases[database]
	if db == nil {
		return nil
	}

	return db.SeriesIDs([]string{measurement}, nil)
}

// measurement returns a measurement by database and name.
func (s *Server) measurement(database, name string) (*Measurement, error) {
	db := s.databases[database]
	if db == nil {
		return nil, ErrDatabaseNotFound
	}

	return db.measurements[name], nil
}

// NormalizeQuery updates all measurements and fields to be fully qualified.
// Uses db as the default database, where applicable.
func (s *Server) NormalizeQuery(q *influxql.Query, defaultDatabase string) error {
	s.mu.RLock()
	defer s.mu.RUnlock()

	for _, stmt := range q.Statements {
		if err := s.normalizeStatement(stmt, defaultDatabase); err != nil {
			return err
		}
	}
	return nil
}

func (s *Server) normalizeStatement(stmt influxql.Statement, defaultDatabase string) (err error) {
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
		case writeSeriesMessageType:
			err = s.applyWriteSeries(m)
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
		case setDefaultRetentionPolicyMessageType:
			err = s.applySetDefaultRetentionPolicy(m)
		case createSeriesIfNotExistsMessageType:
			err = s.applyCreateSeriesIfNotExists(m)
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

// Results represents a list of statement results.
type Results []*Result

// Error returns the first error from any statement.
// Returns nil if no errors occurred on any statements.
func (a Results) Error() error {
	for _, r := range a {
		if r.Err != nil {
			return r.Err
		}
	}
	return nil
}

// MessagingClient represents the client used to receive messages from brokers.
type MessagingClient interface {
	// Publishes a message to the broker.
	Publish(m *messaging.Message) (index uint64, err error)

	// Creates a new replica with a given ID on the broker.
	CreateReplica(replicaID uint64) error

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

// BcryptCost is the cost associated with generating password with Bcrypt.
// This setting is lowered during testing to improve test suite performance.
var BcryptCost = 10

// User represents a user account on the system.
// It can be given read/write permissions to individual databases.
type User struct {
	Name  string `json:"name"`
	Hash  string `json:"hash"`
	Admin bool   `json:"admin,omitempty"`
}

// Authenticate returns nil if the password matches the user's password.
// Returns an error if the password was incorrect.
func (u *User) Authenticate(password string) error {
	return bcrypt.CompareHashAndPassword([]byte(u.Hash), []byte(password))
}

// users represents a list of users, sortable by name.
type users []*User

func (p users) Len() int           { return len(p) }
func (p users) Less(i, j int) bool { return p[i].Name < p[j].Name }
func (p users) Swap(i, j int)      { p[i], p[j] = p[j], p[i] }

type Matcher struct {
	IsRegex bool
	Name    string
}

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
	ID    uint32
	Query string
	// TODO: ParsedQuery *parser.SelectQuery
}

// copyURL returns a copy of the the URL.
func copyURL(u *url.URL) *url.URL {
	other := &url.URL{}
	*other = *u
	return other
}
