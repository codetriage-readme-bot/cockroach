// Copyright 2019 The Cockroach Authors.
//
// Use of this software is governed by the Business Source License included
// in the file licenses/BSL.txt and at www.mariadb.com/bsl11.
//
// Change Date: 2022-10-01
//
// On the date above, in accordance with the Business Source License, use
// of this software will be governed by the Apache License, Version 2.0,
// included in the file licenses/APL.txt and at
// https://www.apache.org/licenses/LICENSE-2.0

package row

import (
	"errors"
	"sort"

	"github.com/cockroachdb/cockroach/pkg/internal/client"
	"github.com/cockroachdb/cockroach/pkg/sql/pgwire/pgerror"
	"github.com/cockroachdb/cockroach/pkg/sql/sqlbase"
)

// fkExistenceCheckBaseHelper is an auxiliary struct that facilitates FK existence
// checks for one FK constraint.
//
// TODO(knz): the fact that this captures the txn is problematic. The
// txn should be passed as argument.
type fkExistenceCheckBaseHelper struct {
	// txn is the current KV transaction.
	txn *client.Txn

	// dir indicates the direction of the check.
	//
	// Note that the helper is used both for forward checks (when
	// INSERTing data in a *referencing* table) and backward checks
	// (when DELETEing data in a *referenced* table). UPDATE uses
	// helpers in both directions.
	//
	// Because it serves both directions, the explanations below
	// avoid using the words "referencing" and "referenced". Instead
	// it uses "searched" for the table/index where the existence
	// is tested; and "mutated" for the table/index being written to.
	//
	dir FKCheckType

	// rf is the row fetcher used to look up rows in the searched table.
	rf *Fetcher

	// searchIdx is the index used for lookups over the searched table.
	searchIdx *sqlbase.IndexDescriptor

	// prefixLen is the number of columns being looked up. In the common
	// case it matches the number of columns in searchIdx, however it is
	// possible to run a lookup check for a prefix of the columns in
	// searchIdx, eg. `(a,b) REFERENCES t(x,y)` with an index on
	// `(x,y,z)`.
	prefixLen int

	// Pre-computed KV key prefix for searchIdx.
	searchPrefix []byte

	// ids maps column IDs in index searchIdx to positions of the `row`
	// array provided to each FK existence check. This tells the checker
	// where to find the values in the row for each column of the
	// searched index.
	ids map[sqlbase.ColumnID]int

	// ref is a copy of the ForeignKeyReference object in the table
	// descriptor.  During the check this is used to decide how to check
	// the value (MATCH style).
	//
	// TODO(knz): the entire reference object is not needed during the
	// mutation, only the match style. Simplify this.
	ref sqlbase.ForeignKeyReference

	// searchTable is the descriptor of the searched table. Stored only
	// for error messages; lookups use the pre-computed searchPrefix.
	searchTable *sqlbase.ImmutableTableDescriptor
	// mutatedIdx is the descriptor for the target index being mutated.
	// Stored only for error messages.
	mutatedIdx *sqlbase.IndexDescriptor
}

// makeFkExistenceCheckBaseHelper instantiates a FK helper.
//
// - dir is the direction of the check.
//
// - ref is a copy of the FK constraint object that points
//   to the table where to perform the existence check.
//
//   For forward checks, this is a copy of the FK
//   constraint placed on the referencing table.
//   For backward checks, this is a copy of the FK
//   constraint placed as backref on the referenced table.
//
//   This is used to derive the searched table/index,
//   and determine the MATCH style.
//
// - writeIdx is the target index being mutated. This is used
//   to determine prefixLen in combination with searchIdx.
//
// - colMap maps column IDs in the searched index, to positions
//   in the input `row` of datums during the check.
//
// - alloc is a suitable datum allocator used to initialize
//   the row fetcher.
//
// - otherTables is an object that provides schema extraction services.
//   TODO(knz): this should become homogeneous across the 3 packages
//   sql, sqlbase, row. The proliferation is annoying.
func makeFkExistenceCheckBaseHelper(
	txn *client.Txn,
	otherTables FkTableMetadata,
	mutatedIdx *sqlbase.IndexDescriptor,
	ref sqlbase.ForeignKeyReference,
	colMap map[sqlbase.ColumnID]int,
	alloc *sqlbase.DatumAlloc,
	dir FKCheckType,
) (ret fkExistenceCheckBaseHelper, err error) {
	// Look up the searched table.
	searchTable := otherTables[ref.Table].Desc
	if searchTable == nil {
		return ret, pgerror.AssertionFailedf("referenced table %d not in provided table map %+v", ref.Table, otherTables)
	}
	// Look up the searched index.
	searchIdx, err := searchTable.FindIndexByID(ref.Index)
	if err != nil {
		return ret, err
	}

	// Determine the number of columns being looked up.
	prefixLen := len(searchIdx.ColumnIDs)
	if len(mutatedIdx.ColumnIDs) < prefixLen {
		prefixLen = len(mutatedIdx.ColumnIDs)
	}

	// Determine the columns being looked up.
	ids, err := computeFkCheckColumnIDs(ref.Match, mutatedIdx, searchIdx, colMap, prefixLen)
	if err != nil {
		return ret, err
	}

	// Precompute the KV lookup prefix.
	searchPrefix := sqlbase.MakeIndexKeyPrefix(searchTable.TableDesc(), ref.Index)

	// Initialize the row fetcher.
	tableArgs := FetcherTableArgs{
		Desc:             searchTable,
		Index:            searchIdx,
		ColIdxMap:        searchTable.ColumnIdxMap(),
		IsSecondaryIndex: searchIdx.ID != searchTable.PrimaryIndex.ID,
		Cols:             searchTable.Columns,
	}
	rf := &Fetcher{}
	if err := rf.Init(
		false /* reverse */, false /* returnRangeInfo */, false /* isCheck */, alloc, tableArgs); err != nil {
		return ret, err
	}

	return fkExistenceCheckBaseHelper{
		txn:          txn,
		dir:          dir,
		rf:           rf,
		ref:          ref,
		searchTable:  searchTable,
		searchIdx:    searchIdx,
		ids:          ids,
		prefixLen:    prefixLen,
		searchPrefix: searchPrefix,
		mutatedIdx:   mutatedIdx,
	}, nil
}

// computeFkCheckColumnIDs determines the set of column IDs to use for
// the existence check, depending on the MATCH style.
//
// See https://github.com/cockroachdb/cockroach/issues/20305 or
// https://www.postgresql.org/docs/11/sql-createtable.html for details on the
// different composite foreign key matching methods.
func computeFkCheckColumnIDs(
	match sqlbase.ForeignKeyReference_Match,
	mutatedIdx *sqlbase.IndexDescriptor,
	searchIdx *sqlbase.IndexDescriptor,
	colMap map[sqlbase.ColumnID]int,
	prefixLen int,
) (ids map[sqlbase.ColumnID]int, err error) {
	ids = make(map[sqlbase.ColumnID]int, len(mutatedIdx.ColumnIDs))

	switch match {
	case sqlbase.ForeignKeyReference_SIMPLE:
		for i, writeColID := range mutatedIdx.ColumnIDs[:prefixLen] {
			if found, ok := colMap[writeColID]; ok {
				ids[searchIdx.ColumnIDs[i]] = found
			} else {
				return nil, errSkipUnusedFK
			}
		}
		return ids, nil

	case sqlbase.ForeignKeyReference_FULL:
		var missingColumns []string
		for i, writeColID := range mutatedIdx.ColumnIDs[:prefixLen] {
			if found, ok := colMap[writeColID]; ok {
				ids[searchIdx.ColumnIDs[i]] = found
			} else {
				missingColumns = append(missingColumns, mutatedIdx.ColumnNames[i])
			}
		}

		switch len(missingColumns) {
		case 0:
			return ids, nil

		case 1:
			return nil, pgerror.Newf(pgerror.CodeForeignKeyViolationError,
				"missing value for column %q in multi-part foreign key", missingColumns[0])

		case prefixLen:
			// All the columns are nulls, don't check the foreign key.
			return nil, errSkipUnusedFK

		default:
			sort.Strings(missingColumns)
			return nil, pgerror.Newf(pgerror.CodeForeignKeyViolationError,
				"missing values for columns %q in multi-part foreign key", missingColumns)
		}

	case sqlbase.ForeignKeyReference_PARTIAL:
		return nil, pgerror.UnimplementedWithIssue(20305, "MATCH PARTIAL not supported")

	default:
		return nil, pgerror.AssertionFailedf("unknown composite key match type: %v", match)
	}
}

var errSkipUnusedFK = errors.New("no columns involved in FK included in writer")
