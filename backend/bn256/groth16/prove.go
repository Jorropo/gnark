// Copyright 2020 ConsenSys AG
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Code generated by gnark/internal/generators DO NOT EDIT

package groth16

import (
	"math/big"

	curve "github.com/consensys/gurvy/bn256"
	"github.com/consensys/gurvy/bn256/fr"

	backend_bn256 "github.com/consensys/gnark/backend/bn256"

	"runtime"
	"sync"

	"github.com/consensys/gnark/internal/utils/debug"
	"github.com/consensys/gnark/internal/utils/parallel"
)

// Proof represents a Groth16 proof that was encoded with a ProvingKey and can be verified
// with a valid statement and a VerifyingKey
type Proof struct {
	Ar, Krs curve.G1Affine
	Bs      curve.G2Affine
}

var (
	minusTwoInv fr.Element
	nnn         int
)

func init() {
	minusTwoInv.SetUint64(2)
	minusTwoInv.Neg(&minusTwoInv).
		Inverse(&minusTwoInv)
	nnn = (runtime.NumCPU() / 2) + 1
}

// Prove creates proof from a circuit
func Prove(r1cs *backend_bn256.R1CS, pk *ProvingKey, solution map[string]interface{}) (*Proof, error) {
	nbPrivateWires := r1cs.NbWires - r1cs.NbPublicWires

	// fft domain (computeH)
	fftDomain := backend_bn256.NewDomain(r1cs.NbConstraints)

	// sample random r and s
	var r, s big.Int
	var _r, _s, _kr fr.Element
	_r.SetRandom()
	_s.SetRandom()
	_kr.Mul(&_r, &_s).Neg(&_kr)

	_r.FromMont()
	_s.FromMont()
	_kr.FromMont()
	_r.ToBigInt(&r)
	_s.ToBigInt(&s)

	// Solve the R1CS and compute the a, b, c vectors
	wireValues := make([]fr.Element, r1cs.NbWires)
	a := make([]fr.Element, r1cs.NbConstraints, fftDomain.Cardinality+nbPrivateWires)
	b := make([]fr.Element, r1cs.NbConstraints, fftDomain.Cardinality)
	c := make([]fr.Element, r1cs.NbConstraints, fftDomain.Cardinality)
	err := r1cs.Solve(solution, a, b, c, wireValues)
	if err != nil {
		return nil, err
	}
	// set the wire values in regular form
	for i := 0; i < len(wireValues); i++ {
		wireValues[i].FromMont()
	}

	// async waits
	chHDone := make(chan struct{}, 1)
	chKrsDone := make(chan struct{}, 1)
	chArDone := make(chan struct{}, 1)
	chBs1Done := make(chan struct{}, 1)

	var h []fr.Element

	go func() {
		// H (witness reduction / FFT part)
		h = computeH(a, b, c, fftDomain)
		// h = append(h, wireValues[:nbPrivateWires]...)
		chHDone <- struct{}{}
	}()

	// keep busy while the fft splits its go routines
	deltas := curve.BatchScalarMultiplicationG1(&pk.G1.Delta, []fr.Element{_r, _s, _kr})

	// wait for FFT to end, as it uses (almost) all our CPUs
	<-chHDone

	// compute proof elements
	// 4 multiexp + 1 FFT
	proof := &Proof{}
	var bs1Affine curve.G1Affine

	opt := curve.NewMultiExpOptions(runtime.NumCPU())

	go func() {
		var krs, krs2, p1 curve.G1Jac
		chDone := make(chan struct{}, 1)
		go func() {
			krs2.MultiExp(pk.G1.K[:nbPrivateWires], wireValues[:nbPrivateWires], opt)
			chDone <- struct{}{}
		}()
		krs.MultiExp(pk.G1.Z, h, opt)

		<-chDone
		krs.AddAssign(&krs2)
		krs.AddMixed(&deltas[2])
		<-chArDone
		p1.ScalarMulGLV(&proof.Ar, &s)
		krs.AddAssign(&p1)
		<-chBs1Done
		p1.ScalarMulGLV(&bs1Affine, &r)
		krs.AddAssign(&p1)

		proof.Krs.FromJacobian(&krs)

		chKrsDone <- struct{}{}
	}()

	chBS2Done := make(chan struct{}, 1)
	go func() {
		// Bs2 (1 multi exp G2 - size = len(wires))
		var Bs, deltaS curve.G2Jac

		nn := len(pk.G2.B) / 4
		if nn > 10 {
			chDone1 := make(chan struct{}, 1)
			chDone2 := make(chan struct{}, 1)
			chDone3 := make(chan struct{}, 1)
			var bs1, bs2, bs3 curve.G2Jac
			go func() {
				bs1.MultiExp(pk.G2.B[:nn], wireValues[:nn], opt)
				chDone1 <- struct{}{}
			}()
			go func() {
				bs2.MultiExp(pk.G2.B[nn:nn*2], wireValues[nn:nn*2], opt)
				chDone2 <- struct{}{}
			}()
			go func() {
				bs3.MultiExp(pk.G2.B[nn*2:nn*3], wireValues[nn*2:nn*3], opt)
				chDone3 <- struct{}{}
			}()
			Bs.MultiExp(pk.G2.B[nn*3:], wireValues[nn*3:], opt)

			<-chDone1
			Bs.AddAssign(&bs1)
			<-chDone2
			Bs.AddAssign(&bs2)
			<-chDone3
			Bs.AddAssign(&bs3)
		} else {
			Bs.MultiExp(pk.G2.B[:], wireValues[:], opt)
		}

		deltaS.ScalarMulGLV(&pk.G2.Delta, &s)
		Bs.AddAssign(&deltaS)
		Bs.AddMixed(&pk.G2.Beta)

		proof.Bs.FromJacobian(&Bs)
		chBS2Done <- struct{}{}
	}()

	go func() {
		var bs1 curve.G1Jac
		bs1.MultiExp(pk.G1.B, wireValues, opt)
		bs1.AddMixed(&pk.G1.Beta)
		bs1.AddMixed(&deltas[1])
		bs1Affine.FromJacobian(&bs1)
		chBs1Done <- struct{}{}
	}()

	go func() {
		var ar curve.G1Jac
		ar.MultiExp(pk.G1.A, wireValues, opt)
		ar.AddMixed(&pk.G1.Alpha)
		ar.AddMixed(&deltas[0])
		proof.Ar.FromJacobian(&ar)
		chArDone <- struct{}{}
	}()

	// wait for all parts of the proof to be computed.
	<-chKrsDone
	<-chBS2Done

	return proof, nil
}

func computeH(a, b, c []fr.Element, fftDomain *backend_bn256.Domain) []fr.Element {
	// H part of Krs
	// Compute H (hz=ab-c, where z=-2 on ker X^n+1 (z(x)=x^n-1))
	// 	1 - _a = ifft(a), _b = ifft(b), _c = ifft(c)
	// 	2 - ca = fft_coset(_a), ba = fft_coset(_b), cc = fft_coset(_c)
	// 	3 - h = ifft_coset(ca o cb - cc)

	n := len(a)
	debug.Assert((n == len(b)) && (n == len(c)))

	// add padding
	padding := make([]fr.Element, fftDomain.Cardinality-n)
	a = append(a, padding...)
	b = append(b, padding...)
	c = append(c, padding...)
	n = len(a)

	// exptable = scale by inverse of n + coset
	// ifft(a) would normaly do FFT(a, wInv) then scale by CardinalityInv
	// fft_coset(a) would normaly mutliply a with expTable of fftDomain.GeneratorSqRt
	// this pre-computed expTable do both in one pass --> it contains
	// expTable[0] = fftDomain.CardinalityInv
	// expTable[1] = fftDomain.GeneratorSqrt^1 * fftDomain.CardinalityInv
	// expTable[2] = fftDomain.GeneratorSqrt^2 * fftDomain.CardinalityInv
	// ...
	expTable := make([]fr.Element, n)
	expTable[0] = fftDomain.CardinalityInv

	var wgExpTable sync.WaitGroup

	// to ensure the pool is busy while the FFT splits, we schedule precomputation of the exp table
	// before the FFTs
	asyncExpTable(fftDomain.CardinalityInv, fftDomain.GeneratorSqRt, expTable, &wgExpTable)

	var wg sync.WaitGroup
	FFTa := func(s []fr.Element) {
		// FFT inverse
		backend_bn256.FFT(s, fftDomain.GeneratorInv)

		// wait for the expTable to be pre-computed
		// in the nominal case, this is non-blocking as the expTable was scheduled before the FFT
		wgExpTable.Wait()
		parallel.Execute(n, func(start, end int) {
			for i := start; i < end; i++ {
				s[i].MulAssign(&expTable[i])
			}
		})

		// FFT coset
		backend_bn256.FFT(s, fftDomain.Generator)
		wg.Done()
	}
	wg.Add(3)
	go FFTa(a)
	go FFTa(b)
	FFTa(c)

	// wait for first step (ifft + fft_coset) to be done
	wg.Wait()

	// h = ifft_coset(ca o cb - cc)
	// reusing a to avoid unecessary memalloc
	parallel.Execute(n, func(start, end int) {
		for i := start; i < end; i++ {
			a[i].Mul(&a[i], &b[i]).
				SubAssign(&c[i]).
				MulAssign(&minusTwoInv)
		}
	})

	// before computing the ifft_coset, we schedule the expTable precompute of the ifft_coset
	// to ensure the pool is busy while the FFT splits
	// similar reasoning as in ifft pass -->
	// expTable[0] = fftDomain.CardinalityInv
	// expTable[1] = fftDomain.GeneratorSqRtInv^1 * fftDomain.CardinalityInv
	// expTable[2] = fftDomain.GeneratorSqRtInv^2 * fftDomain.CardinalityInv
	asyncExpTable(fftDomain.CardinalityInv, fftDomain.GeneratorSqRtInv, expTable, &wgExpTable)

	// ifft_coset
	backend_bn256.FFT(a, fftDomain.GeneratorInv)

	wgExpTable.Wait() // wait for pre-computation of exp table to be done
	parallel.Execute(n, func(start, end int) {
		for i := start; i < end; i++ {
			a[i].MulAssign(&expTable[i]).FromMont()
		}
	})

	return a
}

func asyncExpTable(scale, w fr.Element, table []fr.Element, wg *sync.WaitGroup) {
	n := len(table)

	// see if it makes sense to parallelize exp tables pre-computation
	interval := (n - 1) / runtime.NumCPU()
	// this ratio roughly correspond to the number of multiplication one can do in place of a Exp operation
	const ratioExpMul = 2400 / 26

	if interval < ratioExpMul {
		wg.Add(1)
		go func() {
			precomputeExpTableChunk(scale, w, 1, table[1:])
			wg.Done()
		}()
	} else {
		// we parallelize
		for i := 1; i < n; i += interval {
			start := i
			end := i + interval
			if end > n {
				end = n
			}
			wg.Add(1)
			go func() {
				precomputeExpTableChunk(scale, w, uint64(start), table[start:end])
				wg.Done()
			}()
		}
	}
}

func precomputeExpTableChunk(scale, w fr.Element, power uint64, table []fr.Element) {
	table[0].Exp(w, new(big.Int).SetUint64(power))
	table[0].MulAssign(&scale)
	for i := 1; i < len(table); i++ {
		table[i].Mul(&table[i-1], &w)
	}
}
