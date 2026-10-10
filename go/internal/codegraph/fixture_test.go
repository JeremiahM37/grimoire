package codegraph

import "strings"

// Fixtures are inline rather than under testdata/: a .go file under testdata is
// ignored by the go tool but still has to parse for the walker tests, and
// keeping each language's sample next to its assertions makes the expected
// lines easy to read.

const goFixture = `package store

import (
	"context"
	fmt2 "fmt"
	"strings"
)

const MaxRows = 10

var defaultName = strings.ToUpper("x")

type Store struct {
	name string
}

type Saver interface {
	Save(ctx context.Context) error
}

type Pair[K comparable, V any] struct{ k K; v V }

func New(name string) *Store {
	fmt2.Println(name)
	return &Store{name: name}
}

func (s *Store) Save(ctx context.Context) error {
	return helper(s.name)
}

func (p *Pair[K, V]) Key() K {
	return p.k
}

func helper(v string) error {
	_ = len(v)
	return fmt2.Errorf("%s", v)
}

func Caller() {
	st := New("a")
	_ = st.Save(context.Background())
	_ = helper("b")
}
`

const pyFixture = `import os
import json as js
from pathlib import Path

class Repo:
    """A repository."""

    def __init__(self, root):
        self.root = root

    async def load(self):
        return Path(self.root)

def top_level(x):
    def inner(y):
        return y
    return inner(x)
`

const tsFixture = `import { Client } from './client';
import type { Opts } from "./opts";
export { Thing } from './thing';
const fs = require('fs');

export interface Shape {
  area(): number;
}

export type Id = string;

export class Circle implements Shape {
  constructor(private r: number) {}

  area(): number {
    return Math.PI * this.r * this.r;
  }
}

export function makeCircle(r: number): Circle {
  return new Circle(r);
}

export const double = (n: number) => n * 2;
const LIMIT = 10;
`

const jsFixture = `const path = require("path");
import { x } from "./x.js";

export function render(node) {
  return node;
}

class Widget {
  mount() {
    if (this.ready) {
      return true;
    }
  }
}

const helper = function () {
  return 1;
};
`

// lineOf is the 1-based line of the first line containing sub. It makes the
// assertions say which declaration they mean, rather than hard-coding numbers
// that break every time the fixture gains a line.
func lineOf(src, sub string) int {
	for i, l := range strings.Split(src, "\n") {
		if strings.Contains(l, sub) {
			return i + 1
		}
	}
	return -1
}
