//go:build linux && cgo && !agent

// Code generated by generate-database from the incus project - DO NOT EDIT.

package cluster

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/mattn/go-sqlite3"
)

var networkForwardObjects = RegisterStmt(`
SELECT networks_forwards.id, networks_forwards.network_id, networks_forwards.node_id, nodes.name AS location, networks_forwards.listen_address, networks_forwards.description, networks_forwards.ports
  FROM networks_forwards
  LEFT JOIN nodes ON networks_forwards.node_id = nodes.id
  ORDER BY networks_forwards.network_id, networks_forwards.listen_address
`)

var networkForwardObjectsByNetworkID = RegisterStmt(`
SELECT networks_forwards.id, networks_forwards.network_id, networks_forwards.node_id, nodes.name AS location, networks_forwards.listen_address, networks_forwards.description, networks_forwards.ports
  FROM networks_forwards
  LEFT JOIN nodes ON networks_forwards.node_id = nodes.id
  WHERE ( networks_forwards.network_id = ? )
  ORDER BY networks_forwards.network_id, networks_forwards.listen_address
`)

var networkForwardObjectsByNetworkIDAndListenAddress = RegisterStmt(`
SELECT networks_forwards.id, networks_forwards.network_id, networks_forwards.node_id, nodes.name AS location, networks_forwards.listen_address, networks_forwards.description, networks_forwards.ports
  FROM networks_forwards
  LEFT JOIN nodes ON networks_forwards.node_id = nodes.id
  WHERE ( networks_forwards.network_id = ? AND networks_forwards.listen_address = ? )
  ORDER BY networks_forwards.network_id, networks_forwards.listen_address
`)

var networkForwardID = RegisterStmt(`
SELECT networks_forwards.id FROM networks_forwards
  WHERE networks_forwards.network_id = ? AND networks_forwards.listen_address = ?
`)

var networkForwardCreate = RegisterStmt(`
INSERT INTO networks_forwards (network_id, node_id, listen_address, description, ports)
  VALUES (?, ?, ?, ?, ?)
`)

var networkForwardUpdate = RegisterStmt(`
UPDATE networks_forwards
  SET network_id = ?, node_id = ?, listen_address = ?, description = ?, ports = ?
 WHERE id = ?
`)

var networkForwardDeleteByNetworkIDAndID = RegisterStmt(`
DELETE FROM networks_forwards WHERE network_id = ? AND id = ?
`)

// networkForwardColumns returns a string of column names to be used with a SELECT statement for the entity.
// Use this function when building statements to retrieve database entries matching the NetworkForward entity.
func networkForwardColumns() string {
	return "networks_forwards.id, networks_forwards.network_id, networks_forwards.node_id, nodes.name AS location, networks_forwards.listen_address, networks_forwards.description, networks_forwards.ports"
}

// getNetworkForwards can be used to run handwritten sql.Stmts to return a slice of objects.
func getNetworkForwards(ctx context.Context, stmt *sql.Stmt, args ...any) ([]NetworkForward, error) {
	objects := make([]NetworkForward, 0)

	dest := func(scan func(dest ...any) error) error {
		n := NetworkForward{}
		var portsStr string
		err := scan(&n.ID, &n.NetworkID, &n.NodeID, &n.Location, &n.ListenAddress, &n.Description, &portsStr)
		if err != nil {
			return err
		}

		err = unmarshalJSON(portsStr, &n.Ports)
		if err != nil {
			return err
		}

		objects = append(objects, n)

		return nil
	}

	err := selectObjects(ctx, stmt, dest, args...)
	if err != nil {
		return nil, fmt.Errorf("Failed to fetch from \"networks_forwards\" table: %w", err)
	}

	return objects, nil
}

// getNetworkForwardsRaw can be used to run handwritten query strings to return a slice of objects.
func getNetworkForwardsRaw(ctx context.Context, db dbtx, sql string, args ...any) ([]NetworkForward, error) {
	objects := make([]NetworkForward, 0)

	dest := func(scan func(dest ...any) error) error {
		n := NetworkForward{}
		var portsStr string
		err := scan(&n.ID, &n.NetworkID, &n.NodeID, &n.Location, &n.ListenAddress, &n.Description, &portsStr)
		if err != nil {
			return err
		}

		err = unmarshalJSON(portsStr, &n.Ports)
		if err != nil {
			return err
		}

		objects = append(objects, n)

		return nil
	}

	err := scan(ctx, db, sql, dest, args...)
	if err != nil {
		return nil, fmt.Errorf("Failed to fetch from \"networks_forwards\" table: %w", err)
	}

	return objects, nil
}

// GetNetworkForwards returns all available network_forwards.
// generator: network_forward GetMany
func GetNetworkForwards(ctx context.Context, db dbtx, filters ...NetworkForwardFilter) (_ []NetworkForward, _err error) {
	defer func() {
		_err = mapErr(_err, "Network_forward")
	}()

	var err error

	// Result slice.
	objects := make([]NetworkForward, 0)

	// Pick the prepared statement and arguments to use based on active criteria.
	var sqlStmt *sql.Stmt
	args := []any{}
	queryParts := [2]string{}

	if len(filters) == 0 {
		sqlStmt, err = Stmt(db, networkForwardObjects)
		if err != nil {
			return nil, fmt.Errorf("Failed to get \"networkForwardObjects\" prepared statement: %w", err)
		}
	}

	for i, filter := range filters {
		if filter.NetworkID != nil && filter.ListenAddress != nil && filter.ID == nil && filter.NodeID == nil {
			args = append(args, []any{filter.NetworkID, filter.ListenAddress}...)
			if len(filters) == 1 {
				sqlStmt, err = Stmt(db, networkForwardObjectsByNetworkIDAndListenAddress)
				if err != nil {
					return nil, fmt.Errorf("Failed to get \"networkForwardObjectsByNetworkIDAndListenAddress\" prepared statement: %w", err)
				}

				break
			}

			query, err := StmtString(networkForwardObjectsByNetworkIDAndListenAddress)
			if err != nil {
				return nil, fmt.Errorf("Failed to get \"networkForwardObjects\" prepared statement: %w", err)
			}

			parts := strings.SplitN(query, "ORDER BY", 2)
			if i == 0 {
				copy(queryParts[:], parts)
				continue
			}

			_, where, _ := strings.Cut(parts[0], "WHERE")
			queryParts[0] += "OR" + where
		} else if filter.NetworkID != nil && filter.ID == nil && filter.NodeID == nil && filter.ListenAddress == nil {
			args = append(args, []any{filter.NetworkID}...)
			if len(filters) == 1 {
				sqlStmt, err = Stmt(db, networkForwardObjectsByNetworkID)
				if err != nil {
					return nil, fmt.Errorf("Failed to get \"networkForwardObjectsByNetworkID\" prepared statement: %w", err)
				}

				break
			}

			query, err := StmtString(networkForwardObjectsByNetworkID)
			if err != nil {
				return nil, fmt.Errorf("Failed to get \"networkForwardObjects\" prepared statement: %w", err)
			}

			parts := strings.SplitN(query, "ORDER BY", 2)
			if i == 0 {
				copy(queryParts[:], parts)
				continue
			}

			_, where, _ := strings.Cut(parts[0], "WHERE")
			queryParts[0] += "OR" + where
		} else if filter.ID == nil && filter.NetworkID == nil && filter.NodeID == nil && filter.ListenAddress == nil {
			return nil, fmt.Errorf("Cannot filter on empty NetworkForwardFilter")
		} else {
			return nil, errors.New("No statement exists for the given Filter")
		}
	}

	// Select.
	if sqlStmt != nil {
		objects, err = getNetworkForwards(ctx, sqlStmt, args...)
	} else {
		queryStr := strings.Join(queryParts[:], "ORDER BY")
		objects, err = getNetworkForwardsRaw(ctx, db, queryStr, args...)
	}

	if err != nil {
		return nil, fmt.Errorf("Failed to fetch from \"networks_forwards\" table: %w", err)
	}

	return objects, nil
}

// GetNetworkForwardConfig returns all available NetworkForward Config
// generator: network_forward GetMany
func GetNetworkForwardConfig(ctx context.Context, db tx, networkForwardID int, filters ...ConfigFilter) (_ map[string]string, _err error) {
	defer func() {
		_err = mapErr(_err, "Network_forward")
	}()

	networkForwardConfig, err := GetConfig(ctx, db, "networks_forwards", "network_forward", filters...)
	if err != nil {
		return nil, err
	}

	config, ok := networkForwardConfig[networkForwardID]
	if !ok {
		config = map[string]string{}
	}

	return config, nil
}

// GetNetworkForward returns the network_forward with the given key.
// generator: network_forward GetOne
func GetNetworkForward(ctx context.Context, db dbtx, networkID int64, listenAddress string) (_ *NetworkForward, _err error) {
	defer func() {
		_err = mapErr(_err, "Network_forward")
	}()

	filter := NetworkForwardFilter{}
	filter.NetworkID = &networkID
	filter.ListenAddress = &listenAddress

	objects, err := GetNetworkForwards(ctx, db, filter)
	if err != nil {
		return nil, fmt.Errorf("Failed to fetch from \"networks_forwards\" table: %w", err)
	}

	switch len(objects) {
	case 0:
		return nil, ErrNotFound
	case 1:
		return &objects[0], nil
	default:
		return nil, fmt.Errorf("More than one \"networks_forwards\" entry matches")
	}
}

// GetNetworkForwardID return the ID of the network_forward with the given key.
// generator: network_forward ID
func GetNetworkForwardID(ctx context.Context, db tx, networkID int64, listenAddress string) (_ int64, _err error) {
	defer func() {
		_err = mapErr(_err, "Network_forward")
	}()

	stmt, err := Stmt(db, networkForwardID)
	if err != nil {
		return -1, fmt.Errorf("Failed to get \"networkForwardID\" prepared statement: %w", err)
	}

	row := stmt.QueryRowContext(ctx, networkID, listenAddress)
	var id int64
	err = row.Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return -1, ErrNotFound
	}

	if err != nil {
		return -1, fmt.Errorf("Failed to get \"networks_forwards\" ID: %w", err)
	}

	return id, nil
}

// CreateNetworkForward adds a new network_forward to the database.
// generator: network_forward Create
func CreateNetworkForward(ctx context.Context, db dbtx, object NetworkForward) (_ int64, _err error) {
	defer func() {
		_err = mapErr(_err, "Network_forward")
	}()

	args := make([]any, 5)

	// Populate the statement arguments.
	args[0] = object.NetworkID
	args[1] = object.NodeID
	args[2] = object.ListenAddress
	args[3] = object.Description
	marshaledPorts, err := marshalJSON(object.Ports)
	if err != nil {
		return -1, err
	}

	args[4] = marshaledPorts

	// Prepared statement to use.
	stmt, err := Stmt(db, networkForwardCreate)
	if err != nil {
		return -1, fmt.Errorf("Failed to get \"networkForwardCreate\" prepared statement: %w", err)
	}

	// Execute the statement.
	result, err := stmt.Exec(args...)
	var sqliteErr sqlite3.Error
	if errors.As(err, &sqliteErr) {
		if sqliteErr.Code == sqlite3.ErrConstraint {
			return -1, ErrConflict
		}
	}

	if err != nil {
		return -1, fmt.Errorf("Failed to create \"networks_forwards\" entry: %w", err)
	}

	id, err := result.LastInsertId()
	if err != nil {
		return -1, fmt.Errorf("Failed to fetch \"networks_forwards\" entry ID: %w", err)
	}

	return id, nil
}

// CreateNetworkForwardConfig adds new network_forward Config to the database.
// generator: network_forward Create
func CreateNetworkForwardConfig(ctx context.Context, db dbtx, networkForwardID int64, config map[string]string) (_err error) {
	defer func() {
		_err = mapErr(_err, "Network_forward")
	}()

	referenceID := int(networkForwardID)
	for key, value := range config {
		insert := Config{
			ReferenceID: referenceID,
			Key:         key,
			Value:       value,
		}

		err := CreateConfig(ctx, db, "networks_forwards", "network_forward", insert)
		if err != nil {
			return fmt.Errorf("Insert Config failed for NetworkForward: %w", err)
		}

	}

	return nil
}

// UpdateNetworkForward updates the network_forward matching the given key parameters.
// generator: network_forward Update
func UpdateNetworkForward(ctx context.Context, db tx, networkID int64, listenAddress string, object NetworkForward) (_err error) {
	defer func() {
		_err = mapErr(_err, "Network_forward")
	}()

	id, err := GetNetworkForwardID(ctx, db, networkID, listenAddress)
	if err != nil {
		return err
	}

	stmt, err := Stmt(db, networkForwardUpdate)
	if err != nil {
		return fmt.Errorf("Failed to get \"networkForwardUpdate\" prepared statement: %w", err)
	}

	marshaledPorts, err := marshalJSON(object.Ports)
	if err != nil {
		return err
	}

	result, err := stmt.Exec(object.NetworkID, object.NodeID, object.ListenAddress, object.Description, marshaledPorts, id)
	if err != nil {
		return fmt.Errorf("Update \"networks_forwards\" entry failed: %w", err)
	}

	n, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("Fetch affected rows: %w", err)
	}

	if n != 1 {
		return fmt.Errorf("Query updated %d rows instead of 1", n)
	}

	return nil
}

// UpdateNetworkForwardConfig updates the network_forward Config matching the given key parameters.
// generator: network_forward Update
func UpdateNetworkForwardConfig(ctx context.Context, db tx, networkForwardID int64, config map[string]string) (_err error) {
	defer func() {
		_err = mapErr(_err, "Network_forward")
	}()

	err := UpdateConfig(ctx, db, "networks_forwards", "network_forward", int(networkForwardID), config)
	if err != nil {
		return fmt.Errorf("Replace Config for NetworkForward failed: %w", err)
	}

	return nil
}

// DeleteNetworkForward deletes the network_forward matching the given key parameters.
// generator: network_forward DeleteOne-by-NetworkID-and-ID
func DeleteNetworkForward(ctx context.Context, db dbtx, networkID int64, id int64) (_err error) {
	defer func() {
		_err = mapErr(_err, "Network_forward")
	}()

	stmt, err := Stmt(db, networkForwardDeleteByNetworkIDAndID)
	if err != nil {
		return fmt.Errorf("Failed to get \"networkForwardDeleteByNetworkIDAndID\" prepared statement: %w", err)
	}

	result, err := stmt.Exec(networkID, id)
	if err != nil {
		return fmt.Errorf("Delete \"networks_forwards\": %w", err)
	}

	n, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("Fetch affected rows: %w", err)
	}

	if n == 0 {
		return ErrNotFound
	} else if n > 1 {
		return fmt.Errorf("Query deleted %d NetworkForward rows instead of 1", n)
	}

	return nil
}
