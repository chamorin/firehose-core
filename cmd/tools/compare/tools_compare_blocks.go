// Copyright 2021 dfuse Platform Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package compare

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"reflect"
	"strconv"
	"sync"

	jd "github.com/josephburnett/jd/lib"
	"github.com/spf13/cobra"
	"github.com/streamingfast/bstream"
	"github.com/streamingfast/cli"
	"github.com/streamingfast/cli/sflags"
	"github.com/streamingfast/dstore"
	firecore "github.com/streamingfast/firehose-core"
	"github.com/streamingfast/firehose-core/cmd/tools/check"
	"github.com/streamingfast/firehose-core/types"
	"go.uber.org/multierr"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
)

func NewToolsCompareBlocksCmd[B firecore.Block](chain *firecore.Chain[B]) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "compare-blocks <reference_blocks_store> <current_blocks_store> [<block_range>]",
		Short: "Checks for any differences between two block stores between a specified range. (To compare the likeness of two block ranges, for example)",
		Long: cli.Dedent(`
			The 'compare-blocks' takes in two paths to stores of merged blocks and a range specifying the blocks you
			want to compare, written as: '<start>:<finish>'. It will output the status of the likeness of every
			100,000 blocks, on completion, or on encountering a difference. Increments that contain a difference will
			be communicated as well as the blocks within that contain differences. Increments that do not have any
			differences will be outputted as identical.

			After passing through the blocks, it will output instructions on how to locate a specific difference
			based on the blocks that were given. This is done by applying the '--diff' flag before your args.

			Commands inputted with '--diff' will display the blocks that have differences, as well as the
			difference.
		`),
		Args: cobra.ExactArgs(3),
		RunE: runCompareBlocksE(chain),
		Example: firecore.ExamplePrefixed(chain, "tools compare-blocks", `
			# Run over full block range
			reference_store/ current_store/ 0:16000000

			# Run over specific block range, displaying differences in blocks
			--diff reference_store/ current_store/ 100:200
		`),
	}

	flags := cmd.PersistentFlags()
	flags.Bool("diff", false, "When activated, difference is displayed for each block with a difference")
	flags.Bool("include-unknown-fields", false, "When activated, the 'unknown fields' in the protobuf message will also be compared. These would not generate any difference when unmarshalled with the current protobuf definition.")

	return cmd
}

func runCompareBlocksE[B firecore.Block](chain *firecore.Chain[B]) firecore.CommandExecutor {
	sanitizer := chain.Tools.GetSanitizeBlockForCompare()

	return func(cmd *cobra.Command, args []string) error {
		displayDiff := sflags.MustGetBool(cmd, "diff")
		ignoreUnknown := !sflags.MustGetBool(cmd, "include-unknown-fields")
		segmentSize := uint64(100000)
		warnAboutExtraBlocks := sync.Once{}

		ctx := cmd.Context()
		blockRange, err := types.GetBlockRangeFromArg(args[2])
		if err != nil {
			return fmt.Errorf("parsing range: %w", err)
		}

		if !blockRange.IsResolved() {
			return fmt.Errorf("invalid block range, you must provide a closed range fully resolved (no negative value)")
		}

		stopBlock := blockRange.GetStopBlockOr(firecore.MaxUint64)

		// Create stores
		storeReference, err := dstore.NewDBinStore(args[0])
		if err != nil {
			return fmt.Errorf("unable to create store at path %q: %w", args[0], err)
		}
		storeCurrent, err := dstore.NewDBinStore(args[1])
		if err != nil {
			return fmt.Errorf("unable to create store at path %q: %w", args[1], err)
		}

		segments, err := blockRange.Split(segmentSize, types.EndBoundaryExclusive)
		if err != nil {
			return fmt.Errorf("unable to split blockrage in segments: %w", err)
		}
		processState := &state{
			segments: segments,
		}

		err = storeReference.Walk(ctx, check.WalkBlockPrefix(blockRange, 100), func(filename string) (err error) {
			fileStartBlock, err := strconv.Atoi(filename)
			if err != nil {
				return fmt.Errorf("parsing filename: %w", err)
			}

			// If reached end of range
			if stopBlock <= uint64(fileStartBlock) {
				return dstore.StopIteration
			}

			if blockRange.Contains(uint64(fileStartBlock), types.EndBoundaryExclusive) {
				var wg sync.WaitGroup
				var bundleErrLock sync.Mutex
				var bundleReadErr error
				var referenceBlockHashes []string
				var referenceBlocks map[string]B
				var currentBlocks map[string]B

				wg.Add(1)
				go func() {
					defer wg.Done()
					referenceBlockHashes, referenceBlocks, err = readBundle[B](
						ctx,
						filename,
						storeReference,
						uint64(fileStartBlock),
						stopBlock,
						sanitizer,
						&warnAboutExtraBlocks,
						chain.BlockFactory,
					)
					if err != nil {
						bundleErrLock.Lock()
						bundleReadErr = multierr.Append(bundleReadErr, err)
						bundleErrLock.Unlock()
					}
				}()

				wg.Add(1)
				go func() {
					defer wg.Done()
					_, currentBlocks, err = readBundle(ctx,
						filename,
						storeCurrent,
						uint64(fileStartBlock),
						stopBlock,
						sanitizer,
						&warnAboutExtraBlocks,
						chain.BlockFactory,
					)
					if err != nil {
						bundleErrLock.Lock()
						bundleReadErr = multierr.Append(bundleReadErr, err)
						bundleErrLock.Unlock()
					}
				}()
				wg.Wait()
				if bundleReadErr != nil {
					return fmt.Errorf("reading bundles: %w", bundleReadErr)
				}

				for _, referenceBlockHash := range referenceBlockHashes {
					referenceBlock := referenceBlocks[referenceBlockHash]
					currentBlock, existsInCurrent := currentBlocks[referenceBlockHash]

					var isEqual bool
					if existsInCurrent {
						var differences []string
						isEqual, differences = Compare(referenceBlock, currentBlock, ignoreUnknown)
						if !isEqual {
							fmt.Printf("- Block %s is different\n", firehoseBlockToRef(referenceBlock))
							if displayDiff {
								for _, diff := range differences {
									fmt.Println("  · ", diff)
								}
							}
						}
					}
					processState.process(referenceBlock.GetFirehoseBlockNumber(), !isEqual, !existsInCurrent)
				}
			}
			return nil
		})
		if err != nil {
			return fmt.Errorf("walking files: %w", err)
		}
		processState.print()

		return nil
	}
}

func firehoseBlockToRef[B firecore.Block](b B) bstream.BlockRef {
	return bstream.NewBlockRef(b.GetFirehoseBlockID(), b.GetFirehoseBlockNumber())
}

func readBundle[B firecore.Block](
	ctx context.Context,
	filename string,
	store dstore.Store,
	fileStartBlock,
	stopBlock uint64,
	sanitizer firecore.SanitizeBlockForCompareFunc[B],
	warnAboutExtraBlocks *sync.Once,
	blockFactory func() firecore.Block,
) ([]string, map[string]B, error) {
	fileReader, err := store.OpenObject(ctx, filename)
	if err != nil {
		return nil, nil, fmt.Errorf("creating reader: %w", err)
	}

	blockReader, err := bstream.NewDBinBlockReader(fileReader)
	if err != nil {
		return nil, nil, fmt.Errorf("creating block reader: %w", err)
	}

	var blockHashes []string
	blocksMap := make(map[string]B)
	for {
		curBlock, err := blockReader.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, nil, fmt.Errorf("reading blocks: %w", err)
		}
		if curBlock.Number >= stopBlock {
			break
		}
		if curBlock.Number < fileStartBlock {
			warnAboutExtraBlocks.Do(func() {
				fmt.Printf("Warn: Bundle file %s contains block %d, preceding its start_block. This 'feature' is not used anymore and extra blocks like this one will be ignored during compare\n", store.ObjectURL(filename), curBlock.Number)
			})
			continue
		}

		b := blockFactory()
		if err = curBlock.Payload.UnmarshalTo(b); err != nil {
			fmt.Println("Error unmarshalling block", curBlock.Number, ":", err)
			break
		}

		curBlockPB := sanitizer(b.(B))
		blockHashes = append(blockHashes, curBlock.Id)
		blocksMap[curBlock.Id] = curBlockPB
	}

	return blockHashes, blocksMap, nil
}

type state struct {
	segments                   []types.BlockRange
	currentSegmentIdx          int
	blocksCountedInThisSegment int
	differencesFound           int
	missingBlocks              int
	totalBlocksCounted         int
}

func (s *state) process(blockNum uint64, isDifferent bool, isMissing bool) {
	if !s.segments[s.currentSegmentIdx].Contains(blockNum, types.EndBoundaryExclusive) { // moving forward
		s.print()
		for i := s.currentSegmentIdx; i < len(s.segments); i++ {
			if s.segments[i].Contains(blockNum, types.EndBoundaryExclusive) {
				s.currentSegmentIdx = i
				s.totalBlocksCounted += s.blocksCountedInThisSegment
				s.differencesFound = 0
				s.missingBlocks = 0
				s.blocksCountedInThisSegment = 0
			}
		}
	}

	s.totalBlocksCounted++
	if isMissing {
		s.missingBlocks++
	} else if isDifferent {
		s.differencesFound++
	}

}

func (s *state) print() {
	endBlock := fmt.Sprintf("%d", s.segments[s.currentSegmentIdx].GetStopBlockOr(firecore.MaxUint64))

	if s.totalBlocksCounted == 0 {
		fmt.Printf("✖ No blocks were found at all for segment %d - %s\n", s.segments[s.currentSegmentIdx].Start, endBlock)
		return
	}

	if s.differencesFound == 0 && s.missingBlocks == 0 {
		fmt.Printf("✓ Segment %d - %s has no differences (%d blocks counted)\n", s.segments[s.currentSegmentIdx].Start, endBlock, s.totalBlocksCounted)
		return
	}

	if s.differencesFound == 0 && s.missingBlocks == 0 {
		fmt.Printf("✓~ Segment %d - %s has no differences but does have %d missing blocks (%d blocks counted)\n", s.segments[s.currentSegmentIdx].Start, endBlock, s.missingBlocks, s.totalBlocksCounted)
		return
	}

	fmt.Printf("✖ Segment %d - %s has %d different blocks and %d missing blocks (%d blocks counted)\n", s.segments[s.currentSegmentIdx].Start, endBlock, s.differencesFound, s.missingBlocks, s.totalBlocksCounted)
}

func Compare(reference, current proto.Message, ignoreUnknown bool) (isEqual bool, differences []string) {
	if reference == nil && current == nil {
		return true, nil
	}
	if reflect.TypeOf(reference).Kind() == reflect.Ptr && reference == current {
		return true, nil
	}

	referenceMsg := reference.ProtoReflect()
	currentMsg := current.ProtoReflect()
	if referenceMsg.IsValid() && !currentMsg.IsValid() {
		return false, []string{fmt.Sprintf("reference block is valid protobuf message, but current block is invalid")}
	}
	if !referenceMsg.IsValid() && currentMsg.IsValid() {
		return false, []string{fmt.Sprintf("reference block is invalid protobuf message, but current block is valid")}
	}

	if ignoreUnknown {
		referenceMsg.SetUnknown(nil)
		currentMsg.SetUnknown(nil)
		reference = referenceMsg.Interface().(proto.Message)
		current = currentMsg.Interface().(proto.Message)
	} else {
		x := referenceMsg.GetUnknown()
		y := currentMsg.GetUnknown()

		if !bytes.Equal(x, y) {
			// from https://github.com/protocolbuffers/protobuf-go/tree/v1.28.1/proto
			mx := make(map[protoreflect.FieldNumber]protoreflect.RawFields)
			my := make(map[protoreflect.FieldNumber]protoreflect.RawFields)
			for len(x) > 0 {
				fnum, _, n := protowire.ConsumeField(x)
				mx[fnum] = append(mx[fnum], x[:n]...)
				x = x[n:]
			}
			for len(y) > 0 {
				fnum, _, n := protowire.ConsumeField(y)
				my[fnum] = append(my[fnum], y[:n]...)
				y = y[n:]
			}
			for k, v := range mx {
				vv, ok := my[k]
				if !ok {
					differences = append(differences, fmt.Sprintf("reference block contains unknown protobuf field number %d (%x), but current block does not", k, v))
					continue
				}
				if !bytes.Equal(v, vv) {
					differences = append(differences, fmt.Sprintf("unknown protobuf field number %d has different values. Reference: %x, current: %x", k, v, vv))
				}
			}
			for k := range my {
				v, ok := my[k]
				if !ok {
					differences = append(differences, fmt.Sprintf("current block contains unknown protobuf field number %d (%x), but reference block does not", k, v))
					continue
				}
			}
		}
	}

	if !proto.Equal(reference, current) {
		ref, err := json.MarshalIndent(reference, "", " ")
		cli.NoError(err, "marshal JSON reference")

		cur, err := json.MarshalIndent(current, "", " ")
		cli.NoError(err, "marshal JSON current")

		r, err := jd.ReadJsonString(string(ref))
		cli.NoError(err, "read JSON reference")

		c, err := jd.ReadJsonString(string(cur))
		cli.NoError(err, "read JSON current")

		if diff := r.Diff(c).Render(); diff != "" {
			differences = append(differences, diff)
		}
		return false, differences
	}
	return true, nil
}
