// Copyright 2015 The Cockroach Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or
// implied. See the License for the specific language governing
// permissions and limitations under the License.

// This file implements the SHOW TESTING_RANGES statement:
//   SHOW TESTING_RANGES FROM TABLE t
//   SHOW TESTING_RANGES FROM INDEX t@idx
//
// These statements show the ranges corresponding to the given table or index,
// along with the list of replicas and the lease holder.

package sql

import (
	"sort"

	"golang.org/x/net/context"

	"github.com/cockroachdb/cockroach/pkg/internal/client"
	"github.com/cockroachdb/cockroach/pkg/keys"
	"github.com/cockroachdb/cockroach/pkg/roachpb"
	"github.com/cockroachdb/cockroach/pkg/sql/parser"
	"github.com/cockroachdb/cockroach/pkg/sql/privilege"
	"github.com/cockroachdb/cockroach/pkg/sql/sem/types"
	"github.com/cockroachdb/cockroach/pkg/sql/sqlbase"
	"github.com/pkg/errors"
)

func (p *planner) ShowRanges(ctx context.Context, n *parser.ShowRanges) (planNode, error) {
	tableDesc, index, err := p.getTableAndIndex(ctx, n.Table, n.Index, privilege.SELECT)
	if err != nil {
		return nil, err
	}
	// Note: for interleaved tables, the ranges we report will include rows from
	// interleaving.
	return &showRangesNode{
		span:   tableDesc.IndexSpan(index.ID),
		values: make([]parser.Datum, len(showRangesColumns)),
	}, nil
}

type showRangesNode struct {
	optColumnsSlot

	span roachpb.Span

	// descriptorKVs are KeyValues returned from scanning the
	// relevant meta keys.
	descriptorKVs []client.KeyValue

	rowIdx int
	// values stores the current row, updated by Next().
	values []parser.Datum
}

var showRangesColumns = sqlbase.ResultColumns{
	{
		Name: "Start Key",
		Typ:  types.String,
	},
	{
		Name: "End Key",
		Typ:  types.String,
	},
	{
		Name: "Range ID",
		Typ:  types.Int,
	},
	{
		Name: "Replicas",
		// The INTs in the array are Store IDs.
		Typ: types.TArray{Typ: types.Int},
	},
	{
		Name: "Lease Holder",
		// The store ID for the lease holder.
		Typ: types.Int,
	},
}

func (n *showRangesNode) Start(params runParams) error {
	var err error
	n.descriptorKVs, err = scanMetaKVs(params.ctx, params.p.txn, n.span)
	return err
}

func (n *showRangesNode) Next(params runParams) (bool, error) {
	if n.rowIdx >= len(n.descriptorKVs) {
		return false, nil
	}

	var desc roachpb.RangeDescriptor
	if err := n.descriptorKVs[n.rowIdx].ValueProto(&desc); err != nil {
		return false, err
	}
	for i := range n.values {
		n.values[i] = parser.DNull
	}

	if n.rowIdx > 0 {
		n.values[0] = parser.NewDString(sqlbase.PrettyKey(desc.StartKey.AsRawKey(), 2))
	}

	if n.rowIdx < len(n.descriptorKVs)-1 {
		n.values[1] = parser.NewDString(sqlbase.PrettyKey(desc.EndKey.AsRawKey(), 2))
	}

	n.values[2] = parser.NewDInt(parser.DInt(desc.RangeID))

	var replicas []int
	for _, rd := range desc.Replicas {
		replicas = append(replicas, int(rd.StoreID))
	}
	sort.Ints(replicas)

	replicaArr := parser.NewDArray(types.Int)
	replicaArr.Array = make(parser.Datums, len(replicas))
	for i, r := range replicas {
		replicaArr.Array[i] = parser.NewDInt(parser.DInt(r))
	}
	n.values[3] = replicaArr

	// Get the lease holder.
	// TODO(radu): this will be slow if we have a lot of ranges; find a way to
	// make this part optional.
	b := &client.Batch{}
	b.AddRawRequest(&roachpb.LeaseInfoRequest{
		Span: roachpb.Span{
			Key: desc.StartKey.AsRawKey(),
		},
	})
	if err := params.p.txn.Run(params.ctx, b); err != nil {
		return false, errors.Wrap(err, "error getting lease info")
	}
	resp := b.RawResponse().Responses[0].GetInner().(*roachpb.LeaseInfoResponse)
	n.values[4] = parser.NewDInt(parser.DInt(resp.Lease.Replica.StoreID))

	n.rowIdx++
	return true, nil
}

func (n *showRangesNode) Values() parser.Datums {
	return n.values
}

func (n *showRangesNode) Close(_ context.Context) {
	n.descriptorKVs = nil
}

// scanMetaKVs returns the meta KVs for the ranges that touch the given span.
func scanMetaKVs(
	ctx context.Context, txn *client.Txn, span roachpb.Span,
) ([]client.KeyValue, error) {
	metaStart := keys.RangeMetaKey(keys.MustAddr(span.Key).Next())
	metaEnd := keys.RangeMetaKey(keys.MustAddr(span.EndKey))

	kvs, err := txn.Scan(ctx, metaStart, metaEnd, 0)
	if err != nil {
		return nil, err
	}
	if len(kvs) == 0 || !kvs[len(kvs)-1].Key.Equal(metaEnd.AsRawKey()) {
		// Normally we need to scan one more KV because the ranges are addressed by
		// the end key.
		extraKV, err := txn.Scan(ctx, metaEnd, keys.Meta2Prefix.PrefixEnd(), 1 /* one result */)
		if err != nil {
			return nil, err
		}
		kvs = append(kvs, extraKV[0])
	}
	return kvs, nil
}
