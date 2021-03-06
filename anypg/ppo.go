package anypg

import (
	"github.com/unixpickle/anydiff"
	"github.com/unixpickle/anydiff/anyseq"
	"github.com/unixpickle/anynet"
	"github.com/unixpickle/anyrl"
	"github.com/unixpickle/anyvec"
	"github.com/unixpickle/lazyseq"
)

const DefaultPPOEpsilon = 0.2

// PPOObjective implements the clipped PPO objective
// function defined in: https://arxiv.org/abs/1707.06347.
//
// The eps argument controls how much each ratio is
// encouraged to change (e.g. DefaultPPOEpsilon).
// The rats argument stores ratios between the new action
// probabilities and the original probabilities.
// The advs argument stores an advantage for each ratio.
//
// The output vector contains one component per action.
// Each component is the objective value for that action.
func PPOObjective(eps anyvec.Numeric, rats, advs anydiff.Res) anydiff.Res {
	c := rats.Output().Creator()
	ops := c.NumOps()
	one := c.MakeNumeric(1)
	return anydiff.Pool(rats, func(rats anydiff.Res) anydiff.Res {
		return anydiff.Pool(advs, func(advs anydiff.Res) anydiff.Res {
			clipped := anydiff.ClipRange(rats, ops.Sub(one, eps),
				ops.Add(one, eps))
			return anydiff.ElemMin(
				anydiff.Mul(clipped, advs),
				anydiff.Mul(rats, advs),
			)
		})
	})
}

// PPOTerms represents the current value of the surrogate
// PPO objective function in terms of the advantage,
// critic, and regularization terms.
// The sum of the three terms exactly represents the
// objective function.
type PPOTerms struct {
	MeanAdvantage      anyvec.Numeric
	MeanCritic         anyvec.Numeric
	MeanRegularization anyvec.Numeric
}

// PPO implements Proximal Policy Optimization.
// See https://arxiv.org/abs/1707.06347.
type PPO struct {
	// Params specifies which parameters to include in
	// the gradients.
	Params []*anydiff.Var

	// Base is the part of the agent shared by both the
	// actor and the critic.
	// Its outputs are fed into Actor and Critic.
	//
	// If nil, the identity mapping is used.
	Base func(obses lazyseq.Rereader) lazyseq.Rereader

	// Actor is the policy part of the agent.
	Actor func(baseOut lazyseq.Rereader) lazyseq.Rereader

	// Critic estimates the value function.
	Critic func(baseOut lazyseq.Rereader) lazyseq.Rereader

	// ActionSpace determines log-likelihoods of actions.
	ActionSpace anyrl.LogProber

	// CriticWeight is the importance assigned to the
	// critic's loss during training.
	//
	// If 0, a default of 1 is used.
	CriticWeight float64

	// Regularizer can be used to encourage exploration.
	Regularizer Regularizer

	// Discount is the reward discount factor.
	Discount float64

	// Lambda is the GAE coefficient.
	Lambda float64

	// Epsilon is the amount by which the probability
	// ratio should change.
	//
	// If 0, DefaultPPOEpsilon is used.
	Epsilon float64

	// PoolBase, if true, indicates that the output of the
	// Base function should be pooled to prevent multiple
	// forward/backward Base evaluations.
	//
	// If this is true, then the entire output of Base is
	// stored in memory.
	PoolBase bool
}

// Advantage computes the GAE estimator for a batch.
//
// You should call this once per batch.
// You should not call it between training steps in the
// same batch, since the advantage estimator will change
// as the value function is trained.
func (p *PPO) Advantage(r *anyrl.RolloutSet) lazyseq.Tape {
	judger := &GAEJudger{
		ValueFunc: func(inputs lazyseq.Rereader) <-chan *anyseq.Batch {
			return p.Critic(p.applyBaseIn(inputs)).Forward()
		},
		Discount: p.Discount,
		Lambda:   p.Lambda,
	}
	return judger.JudgeActions(r).Tape(r.Inputs.Creator())
}

// Run computes the gradient for a PPO step.
// It takes a batch of rollouts and the precomputed
// advantages for that batch.
//
// This may be called multiple times per batch to fully
// maximize the objective.
//
// If p.Params is empty, then an empty gradient and nil
// PPOTerms are returned.
func (p *PPO) Run(r *anyrl.RolloutSet, adv lazyseq.Tape) (anydiff.Grad, *PPOTerms) {
	grad := anydiff.NewGrad(p.Params...)
	if len(grad) == 0 {
		return grad, nil
	}
	c := r.Creator()
	targetValues := (&QJudger{Discount: p.Discount}).JudgeActions(r)

	objective := p.runActorCritic(r, func(actor, critic lazyseq.Rereader) anydiff.Res {
		obj := lazyseq.MapN(
			func(n int, v ...anydiff.Res) anydiff.Res {
				actor, critic := v[0], v[1]
				oldOuts, actions := v[2], v[3]
				advantage, targets := v[4], v[5]

				ratios := anydiff.Exp(
					anydiff.Sub(
						p.ActionSpace.LogProb(actor, actions.Output(), n),
						p.ActionSpace.LogProb(oldOuts, actions.Output(), n),
					),
				)
				advTerm := p.clippedObjective(ratios, advantage)

				criticCoeff := -1.0
				if p.CriticWeight != 0 {
					criticCoeff *= p.CriticWeight
				}
				criticTerm := anydiff.Scale(
					anydiff.Square(anydiff.Sub(critic, targets)),
					c.MakeNumeric(criticCoeff),
				)

				var regTerm anydiff.Res
				if p.Regularizer != nil {
					regTerm = p.Regularizer.Regularize(actor, n)
				} else {
					regTerm = anydiff.NewConst(c.MakeVector(n))
				}

				cm := anynet.ConcatMixer{}
				return cm.Mix(cm.Mix(advTerm, criticTerm, n), regTerm, n)
			},
			actor,
			critic,
			lazyseq.TapeRereader(r.AgentOuts),
			lazyseq.TapeRereader(r.Actions),
			lazyseq.TapeRereader(adv),
			lazyseq.TapeRereader(targetValues.Tape(c)),
		)
		return lazyseq.Mean(obj)
	})
	objective.Propagate(anyvec.Ones(c, 3), grad)

	terms := &PPOTerms{
		MeanAdvantage:      anyvec.Sum(objective.Output().Slice(0, 1)),
		MeanCritic:         anyvec.Sum(objective.Output().Slice(1, 2)),
		MeanRegularization: anyvec.Sum(objective.Output().Slice(2, 3)),
	}

	return grad, terms
}

// runActorCritic computes the outputs of the actor and
// the critic and passes them to f.
// It returns the result of f.
//
// This may perform pooling, hence the unusual callback
// setup.
func (p *PPO) runActorCritic(r *anyrl.RolloutSet,
	f func(actor, critic lazyseq.Rereader) anydiff.Res) anydiff.Res {
	if p.PoolBase {
		baseOut := p.applyBase(r)
		return lazyseq.PoolToVec(baseOut, func(baseOut anyseq.Seq) anydiff.Res {
			actorOut := p.Actor(lazyseq.Lazify(baseOut))
			criticOut := p.Critic(lazyseq.Lazify(baseOut))
			return f(actorOut, criticOut)
		})
	} else {
		actorOut := p.Actor(p.applyBase(r))
		criticOut := p.Critic(p.applyBase(r))
		return f(actorOut, criticOut)
	}
}

func (p *PPO) applyBase(r *anyrl.RolloutSet) lazyseq.Rereader {
	return p.applyBaseIn(lazyseq.TapeRereader(r.Inputs))
}

func (p *PPO) applyBaseIn(in lazyseq.Rereader) lazyseq.Rereader {
	if p.Base == nil {
		return in
	} else {
		return p.Base(in)
	}
}

func (p *PPO) clippedObjective(ratios, advantages anydiff.Res) anydiff.Res {
	epsilon := p.Epsilon
	if epsilon == 0 {
		epsilon = DefaultPPOEpsilon
	}
	c := ratios.Output().Creator()
	return PPOObjective(c.MakeNumeric(epsilon), ratios, advantages)
}
