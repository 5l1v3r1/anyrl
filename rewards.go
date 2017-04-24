package anyrl

import (
	"math"

	"github.com/unixpickle/anydiff/anyseq"
	"github.com/unixpickle/anyvec"
	"github.com/unixpickle/lazyseq"
)

// TotalRewards sums the rewards for each sequence of
// rewards.
func TotalRewards(c anyvec.Creator, rewards lazyseq.Tape) anyvec.Vector {
	sum := rewardSum(rewards)
	if sum == nil {
		return c.MakeVector(0)
	}
	return sum.Packed
}

// MeanReward sums the rewards for each sequence, then
// computes the mean of the sums.
func MeanReward(c anyvec.Creator, rewards lazyseq.Tape) anyvec.Numeric {
	total := TotalRewards(c, rewards)
	total.Scale(total.Creator().MakeNumeric(1 / float64(total.Len())))
	return anyvec.Sum(total)
}

// DiscountedRewards computes discounted rewards.
func DiscountedRewards(rewards lazyseq.Tape, factor float64) lazyseq.Tape {
	res, writer := lazyseq.ReferenceTape()

	var i float64
	for in := range rewards.ReadTape(0, -1) {
		discount := math.Pow(factor, i)
		scaled := in.Reduce(in.Present)
		scaled.Packed.Scale(scaled.Packed.Creator().MakeNumeric(discount))
		writer <- scaled
		i++
	}

	close(writer)
	return res
}

func rewardSum(rewards lazyseq.Tape) *anyseq.Batch {
	var sum *anyseq.Batch
	for batch := range rewards.ReadTape(0, -1) {
		if sum == nil {
			sum = &anyseq.Batch{
				Present: batch.Present,
				Packed:  batch.Packed.Copy(),
			}
		} else {
			sum.Packed.Add(batch.Expand(sum.Present).Packed)
		}
	}
	return sum
}