package auth

import (
	"errors"
	"math/big"
)

// maxRhoSteps bounds the rho iteration: legitimate exchange semiprimes
// factor in well under this budget; a hostile pq burns bounded time.
const maxRhoSteps = 1 << 22

// Factorize extracts p and q from the semiprime pq = p*q using Pollard's
// rho with Brent's improvement (reference crypto/factorization.py, which
// seeds its LCG deterministically — mirrored here so factorizations are
// reproducible).
func Factorize(pq *big.Int) (*big.Int, *big.Int, error) {
	if pq.Sign() <= 0 || pq.BitLen() < 4 {
		return nil, nil, errors.New("auth: pq too small to factorize")
	}
	two := big.NewInt(2)
	if new(big.Int).Mod(pq, two).Cmp(big.NewInt(0)) == 0 {
		return two, new(big.Int).Rsh(pq, 1), nil
	}

	// Retry over seeds: a given (y, c) pair can collapse the batch GCD to
	// n itself; standard Brent practice rotates seeds until a nontrivial
	// factor appears. Deterministic seed order keeps results reproducible.
	var divisor *big.Int
	for seed := int64(1); seed <= 8 && divisor == nil; seed++ {
		divisor = brentRhoSeeded(pq, big.NewInt(2), big.NewInt(seed))
	}
	if divisor == nil {
		return nil, nil, errors.New("auth: factorization failed")
	}
	other := new(big.Int).Quo(pq, divisor)
	if divisor.Cmp(other) > 0 {
		divisor, other = other, divisor
	}
	return divisor, other, nil
}

// brentRhoSeeded finds a nontrivial factor of n for a given (y, c) seed,
// or nil. Brent's improvement over Pollard's rho.
func brentRhoSeeded(n, ySeed, cSeed *big.Int) *big.Int {
	one := big.NewInt(1)
	abs := func(x *big.Int) *big.Int {
		if x.Sign() < 0 {
			return new(big.Int).Neg(x)
		}
		return new(big.Int).Abs(x)
	}

	y := new(big.Int).Set(ySeed)
	c := new(big.Int).Set(cSeed)
	m := big.NewInt(128)

	g := new(big.Int).Set(one)
	r := new(big.Int).Set(one)
	q := new(big.Int).Set(one)
	x := new(big.Int)
	ys := new(big.Int)
	tmp := new(big.Int)
	steps := 0

	bump := func(v *big.Int) {
		v.Mul(v, v)
		v.Add(v, c)
		v.Mod(v, n)
		steps++
	}

	for g.Cmp(one) == 0 {
		x.Set(y)
		for i := big.NewInt(0); i.Cmp(r) < 0; i.Add(i, one) {
			bump(y)
			if steps > maxRhoSteps {
				return nil
			}
		}
		k := big.NewInt(0)
		for k.Cmp(r) < 0 && g.Cmp(one) == 0 {
			ys.Set(y)
			lim := m
			if tmp.Sub(r, k).Cmp(m) < 0 {
				lim = tmp
			}
			for i := big.NewInt(0); i.Cmp(lim) < 0; i.Add(i, one) {
				bump(y)
				if steps > maxRhoSteps {
					return nil
				}
				tmp.Sub(x, y)
				q.Mul(q, abs(tmp))
				q.Mod(q, n)
			}
			g.GCD(nil, nil, q, n)
			k.Add(k, m)
		}
		r.Lsh(r, 1)
	}

	if g.Cmp(n) == 0 {
		for {
			bump(ys)
			if steps > maxRhoSteps {
				return nil
			}
			tmp.Sub(x, ys)
			g.GCD(nil, nil, abs(tmp), n)
			if g.Cmp(one) > 0 {
				break
			}
			if ys.Cmp(x) == 0 {
				return nil
			}
		}
	}
	if g.Cmp(n) == 0 || g.Cmp(one) == 0 {
		return nil
	}
	return g
}
