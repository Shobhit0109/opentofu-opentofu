// Copyright (c) The OpenTofu Authors
// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2023 HashiCorp, Inc.
// SPDX-License-Identifier: MPL-2.0

package configschema

import (
	"github.com/zclconf/go-cty/cty"
)

type StringKind int

const (
	StringPlain StringKind = iota
	StringMarkdown
)

// Block represents a configuration block.
//
// "Block" here is a logical grouping construct, though it happens to map
// directly onto the physical block syntax of OpenTofu's native configuration
// syntax. It may be a more a matter of convention in other syntaxes, such as
// JSON.
//
// When converted to a value, a Block always becomes an instance of an object
// type derived from its defined attributes and nested blocks
type Block struct {
	// Attributes describes any attributes that may appear directly inside
	// the block.
	Attributes map[string]*Attribute

	// BlockTypes describes any nested block types that may appear directly
	// inside the block.
	BlockTypes map[string]*NestedBlock

	Description     string
	DescriptionKind StringKind

	Deprecated bool
}

// Attribute represents a configuration attribute, within a block.
type Attribute struct {
	// Type is a type specification that the attribute's value must conform to.
	// It conflicts with NestedType.
	Type cty.Type

	// NestedType indicates that the attribute is a NestedBlock-style object.
	// This field conflicts with Type.
	NestedType *Object

	// Description is an English-language description of the purpose and
	// usage of the attribute. A description should be concise and use only
	// one or two sentences, leaving full definition to longer-form
	// documentation defined elsewhere.
	Description     string
	DescriptionKind StringKind

	// Required, Optional, and Computed together represent how this attribute
	// may be set in the configuration and in responses from a provider.
	//
	// Only the following combinations of true values are valid:
	// - Required: must be set to a non-null value in the configuration.
	// - Optional: may be set to either a null or non-null value in the
	//   configuration. If not set, the value defaults to null.
	// - Computed: may NOT be set in the configuration; the value is decided
	//   only by the provider, typically based on something returned from
	//   the underlying remote API.
	// - Optional+Computed: As with optional, except that if and only if the
	//   configuration causes it to be set to null (either explicitly or by
	//   omission) then the provider decides the final value, such as by
	//   providing a default.
	//
	// All other combinations of these flags are invalid and so have no meaning.
	//
	// The "Computed" flag only applies to schemas used in contexts where a
	// provider returns a value derived from the given configuration. For
	// example, it's used in the schema for a resource type because the
	// provider returns a new result representing a combination of
	// configuration, prior state, and remote API data, but it is not used in
	// the schema for a provider's own configuration because that is used only
	// as an input to the provider and so there is no means for a provider to
	// return a derived object.
	Required, Optional, Computed bool

	// Sensitive, if set to true, indicates that an attribute may contain
	// sensitive information.
	//
	// At present nothing is done with this information, but callers are
	// encouraged to set it where appropriate so that it may be used in the
	// future to help OpenTofu mask sensitive information. (OpenTofu
	// currently achieves this in a limited sense via other mechanisms.)
	Sensitive bool

	Deprecated bool
}

// Object represents the embedding of a structural object inside an Attribute.
type Object struct {
	// Attributes describes the nested attributes which may appear inside the
	// Object.
	Attributes map[string]*Attribute

	// Nesting provides the nesting mode for this Object, which determines how
	// many instances of the Object are allowed, how many labels it expects, and
	// how the resulting data will be converted into a data structure.
	Nesting NestingMode
}

// NestedBlock represents the embedding of one block within another.
type NestedBlock struct {
	// Block is the description of the block that's nested.
	Block

	// Nesting provides the nesting mode for the child block, which determines
	// how many instances of the block are allowed, how many labels it expects,
	// and how the resulting data will be converted into a data structure.
	Nesting NestingMode

	// MinItems and MaxItems set, for the NestingList and NestingSet nesting
	// modes, lower and upper limits on the number of child blocks allowed
	// of the given type. If both are left at zero, no limit is applied.
	//
	// As a special case, both values can be set to 1 for NestingSingle in
	// order to indicate that a particular single block is required.
	//
	// These fields are ignored for other nesting modes and must both be left
	// at zero.
	MinItems, MaxItems int
}

// NestingMode is an enumeration of modes for nesting blocks inside other
// blocks.
type NestingMode int

// Object represents the embedding of a NestedBl

//go:generate go run golang.org/x/tools/cmd/stringer -type=NestingMode

const (
	nestingModeInvalid NestingMode = iota

	// NestingSingle indicates that only a single instance of a given
	// block type is permitted, with no labels, and its content should be
	// provided directly as an object value.
	NestingSingle

	// NestingGroup is similar to NestingSingle in that it calls for only a
	// single instance of a given block type with no labels, but it additionally
	// guarantees that its result will never be null, even if the block is
	// absent, and instead the nested attributes and blocks will be treated
	// as absent in that case. (Any required attributes or blocks within the
	// nested block are not enforced unless the block is explicitly present
	// in the configuration, so they are all effectively optional when the
	// block is not present.)
	//
	// This is useful for the situation where a remote API has a feature that
	// is always enabled but has a group of settings related to that feature
	// that themselves have default values. By using NestingGroup instead of
	// NestingSingle in that case, generated plans will show the block as
	// present even when not present in configuration, thus allowing any
	// default values within to be displayed to the user.
	NestingGroup

	// NestingList indicates that multiple blocks of the given type are
	// permitted, with no labels, and that their corresponding objects should
	// be provided in a list.
	NestingList

	// NestingSet indicates that multiple blocks of the given type are
	// permitted, with no labels, and that their corresponding objects should
	// be provided in a set.
	NestingSet

	// NestingMap indicates that multiple blocks of the given type are
	// permitted, each with a single label, and that their corresponding
	// objects should be provided in a map whose keys are the labels.
	//
	// It's an error, therefore, to use the same label value on multiple
	// blocks.
	NestingMap
)
