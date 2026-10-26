/**
 * The local store and the reads that keep themselves up to date.
 *
 * `entities` is generated -- one declaration per entity, and no behaviour. The
 * runtime reads those together with the protobuf descriptors and implements
 * normalizing, reconciling and applying a `Watch` once, for every entity there
 * is.
 *
 * `Queries` is the half above it. A read that goes through it is a read the
 * framework knows about, so when a row it answered with changes, everything
 * drawing that row is told -- and every place it appears changes at once, with
 * nothing declared anywhere. `@lesomnus/payday/react` is twenty lines of
 * `useSyncExternalStore` over this; Vue or Svelte is the same file, the same
 * length.
 *
 * @module
 */

import type { Transport } from '@connectrpc/connect'

import { Queries } from '@lesomnus/payday/query'
import type { App } from '@lesomnus/payday/react'
import { Store } from '@lesomnus/payday/store'
import { deleteOtherDisks, openDisk } from '@lesomnus/payday/store/idb'

import { entities } from '../gen/entities.js'

/**
 * open answers with this app's store and queries, for one caller.
 *
 * `identity` names what the caller may see, rather than the person -- see
 * `identityOf`. What a caller may see is the actor and the scope together, so
 * the same person holding a narrowed credential is a different store; keyed on
 * the person, the narrowed session would draw the wide session's rows. Nothing
 * leaks either way, and the screen is wrong.
 *
 * It is taken already made, because the two pages `pd new` writes come by it
 * differently: `main.tsx` holds its credential and digests it, and
 * `session.tsx`, signed in by a cookie it cannot read, is told the name by the
 * server (`GET /session`) -- one name for every sign-in that sees the same
 * things, so a session that idled out is the same store once its person signs
 * in again.
 *
 * The mirror is what makes a reload draw the page it had rather than a spinner
 * for it. Delete the disk and everything still works -- the store is a store
 * either way, and it lives as long as the tab does.
 *
 * It expires after a week; `openDisk(entities, at, { keep })` is the knob, and
 * `Infinity` turns it off. What it bounds is both how large a mirror gets and
 * how old the answer a reload draws may be.
 *
 * And once this caller's mirror is open, every other caller's goes
 * (`deleteOtherDisks`). A mirror is swept only when its own caller opens it
 * again, so one left by somebody who stopped using this browser would keep
 * their rows, answers and unsent writes for as long as the browser keeps the
 * database: they are not owed what they left here, and whoever uses it now is
 * owed not finding it. Not waited on -- a database another page holds open
 * answers late, and the page draws meanwhile.
 */
export async function open(transport: Transport, identity: string): Promise<App> {
	const at = { name: 'page', identity }

	const store = Store.open(entities, { ...at, disk: await openDisk(entities, at) })
	void deleteOtherDisks(at)
	await store.hydrate()

	return { store, queries: new Queries(store, transport, entities) }
}
