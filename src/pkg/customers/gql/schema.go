package gql

// Schema is the customer graph, read-only: there is no Mutation and no
// Subscription type, so nothing can be written through this endpoint.
const Schema = `
schema {
	query: Query
}

# The customer graph: people, the artefacts they touched, and the edges
# between them. Read-only. Every field that returns a person checks who is
# asking; a read-only user reaches only their own profile.
type Query {
	# The viewer's own profile.
	me: Person!
	# One profile. null when there is no such person or the viewer may not
	# see them; the two cases are not distinguishable.
	person(id: ID!): Person
	# Customers the viewer may see, by name. Suggested people are never
	# returned.
	people(tag: String, first: Int, after: String): PersonConnection!
	# An artefact by reference, for walking back to the people on it. null
	# when the reference is malformed or the viewer may not see it.
	artefact(ref: String!): Artefact
}

type Person {
	id: ID!
	name: String!
	origin: String!
	me: Boolean!
	tags: [Tag!]!
	identities: [Identity!]!
	goals: [Goal!]!
	# null for a viewer without the full view.
	satisfaction: Satisfaction
	# null for a viewer without the full view.
	notes: String
	counts: [Count!]!
	edges(predicate: String, first: Int, after: String): EdgeConnection!
}

type PersonConnection {
	nodes: [Person!]!
	# Cursor for the next page; null on the last page.
	next: String
}

type Tag {
	name: String!
	on: Boolean!
	# "auto" or "manual".
	source: String!
	rule: String
}

type Identity {
	kind: String!
	value: String!
	source: String
	manual: Boolean!
	# true while a read-only user's own addition waits to be confirmed.
	claimed: Boolean!
}

type Goal {
	id: ID!
	text: String!
	# "suggested" or "confirmed".
	state: String!
	# "auto" (inferred) or "manual".
	source: String!
	evidence: [String!]!
}

type Satisfaction {
	# unknown, at_risk, neutral or satisfied. There is no score.
	level: String!
	source: String
	evidence: [String!]!
	suggestedLevel: String
	suggestedEvidence: [String!]!
}

type Count {
	predicate: String!
	count: Int!
}

type Edge {
	predicate: String!
	at: String!
	source: String
	artefact: Artefact!
}

type EdgeConnection {
	nodes: [Edge!]!
	# Cursor for the next page; null on the last page.
	next: String
}

# An artefact is a reference, never content.
type Artefact {
	ref: String!
	kind: String!
	# The people on this artefact that the viewer may see.
	people(first: Int): [ArtefactPerson!]!
}

type ArtefactPerson {
	predicate: String!
	person: Person!
}
`
