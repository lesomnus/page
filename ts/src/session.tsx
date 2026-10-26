/**
 * Where the page starts, signed in by a session cookie.
 *
 * `main.tsx` holds its credential, which is what `auth.Plain` needs. This is
 * the same page once `serve.go` serves `auth/authsession` instead: the cookie
 * is HttpOnly, so nothing here ever holds a credential -- it asks the server
 * which session the browser holds, and signs in by posting to the endpoint that
 * sets the cookie. Point `index.html` at this file then, and `main.tsx` can go.
 *
 * What the server has to serve, in `serve.go`:
 *
 *     h.Handle("GET /session", sessions.Serve(login))
 *     h.Handle("POST /session", sessions.Serve(login))
 *     h.Handle("DELETE /session", sessions.Serve(login))
 *
 * Over plain HTTP -- every `npm run dev` -- the sessions are made with
 * `authsession.Insecure()`, since a browser will not keep a `Secure` cookie it
 * was handed over `http://`.
 *
 * # The cookie is every tab's
 *
 * Whoever signed in last, in any tab, is who every page of this origin sends
 * as. The query layer says which store each call is for, and the server
 * refuses a call the cookie is not that store's session for -- so a page left
 * holding one person's store never reads the next person's rows into it, or
 * sends what it was keeping as them. What is left to the page is noticing:
 *
 *   - A call refused `Unauthenticated` may be a session that ended, or one
 *     that is somebody else's now. The page asks again who is signed in, and
 *     if it is not this store's caller it lets go of the store. It does not
 *     forget it: a session that idled out is the same person once they sign in
 *     again, the same store, and what they wrote offline goes then.
 *   - Signing out, in any tab, is said to every other on a `BroadcastChannel`,
 *     and each forgets what it was holding for that person -- the rows, the
 *     answers, and the writes they never sent.
 *
 * @module
 */

import { Code, ConnectError, createClient, type Transport } from '@connectrpc/connect'
import { createConnectTransport } from '@connectrpc/connect-web'
import { StrictMode, useState } from 'react'
import { createRoot } from 'react-dom/client'

import { parse } from '@lesomnus/payday/pdid'
import { Provider } from '@lesomnus/payday/react'

import { HolderService } from '../gen/app/payday/holder_svc_pb.js'
import { events } from '../gen/events.js'
import { open } from './store.js'
import { Page } from './page.js'
import './style.css'

/** Where the app answers; see `main.tsx`. */
const ADDR = import.meta.env['VITE_ADDR'] ?? 'http://localhost:8080'

const root = createRoot(document.getElementById('root') as HTMLElement)

/**
 * Every request carries the cookie, which a page on another origin -- which
 * is every `npm run dev` -- sends only when it asks to. Left out, signing in
 * works in `curl` and does nothing in a browser, with no error anywhere.
 */
function send(input: RequestInfo | URL, init?: RequestInit): Promise<Response> {
	return fetch(input, { ...init, credentials: 'include' })
}

/**
 * Where a sign-out in any tab is said to every other; see the note at the top.
 * What is said is the identity of the store that was signed out of.
 */
const signedOut = new BroadcastChannel('page/signed-out')

/** The store this page is drawing, and how it lets go of it; see [boot]. */
let drawn: { identity: string; leave: (forget: boolean) => void } | undefined

signedOut.onmessage = (e: MessageEvent<string>): void => {
	if (drawn !== undefined) drawn.leave(e.data === drawn.identity)
}

/**
 * Which session this browser holds, as `GET /session` says it: a name for the
 * store, and who the session names. Null when it holds none.
 *
 * The name is a digest of what the session lets its caller see, made where the
 * session can be read -- not of the person, which `identityOf` says goes
 * wrong, and not of the key, which would leave a store behind every session
 * that ends by itself.
 */
async function which(): Promise<{ identity: string; id: string; tenantId: string } | null> {
	const res = await send(`${ADDR}/session`)
	if (res.status === 401) return null
	if (!res.ok) throw new Error(`cannot tell who is signed in (${res.status})`)

	return (await res.json()) as { identity: string; id: string; tenantId: string }
}

function SignIn(props: { said?: string }): React.ReactNode {
	const [said, setSaid] = useState(props.said ?? '')

	return (
		<form
			className="sign-in"
			onSubmit={(e) => {
				e.preventDefault()
				const f = new FormData(e.currentTarget)

				// The body is whatever this app's `Verify` reads; this is the
				// shape the signing-in guide's example takes.
				send(`${ADDR}/session`, {
					method: 'POST',
					headers: { 'content-type': 'application/json' },
					body: JSON.stringify({ tenant: f.get('tenant'), alias: f.get('alias'), password: f.get('password') }),
				})
					.then((res) => {
						if (res.status === 204) return boot()
						setSaid(res.status === 401 ? 'not signed in: check what you typed' : `cannot sign in just now (${res.status})`)
					})
					.catch(() => setSaid('the server cannot be reached'))
			}}
		>
			<h1>page</h1>
			<label>
				tenant
				<input name="tenant" defaultValue="acme" />
			</label>
			<label>
				who
				<input name="alias" defaultValue="admin" autoFocus />
			</label>
			<label>
				password
				<input name="password" type="password" />
			</label>
			<button type="submit">sign in</button>
			{said !== '' && <p className="bad">{said}</p>}
		</form>
	)
}

/**
 * Who is signed in, asked of the server by the identifiers the session names.
 * The sign-in answered nothing about the person, and on purpose: this is the
 * request a page makes for that, behind the same wall as every other one.
 */
async function whoIs(transport: Transport, id: string): Promise<string> {
	const me = await createClient(HolderService, transport).get({
		ref: { key: { case: 'id', value: parse(id).bytes } },
		select: { all: true, tenant: { all: true } },
	})

	return `@${me.tenant?.alias ?? ''}/${me.alias}`
}

function askToSignIn(said?: string): void {
	root.render(<StrictMode>{said === undefined ? <SignIn /> : <SignIn said={said} />}</StrictMode>)
}

async function boot(): Promise<void> {
	drawn = undefined

	// What a call refused for its session does: nothing until there is a
	// store to let go of. This boot's own, so a call the last store made that
	// comes back late lets go of that store and not of this one.
	let lost = (): void => {}

	// No `authorization` header and no `Plain`: the cookie is the credential,
	// and the browser sends it.
	const transport = createConnectTransport({
		baseUrl: ADDR,
		fetch: send,
		// What this app's events hold, without which Connect's JSON cannot read
		// an Any; see `main.tsx`.
		jsonOptions: { registry: events },
		interceptors: [
			(next) => async (req) => {
				try {
					return await next(req)
				} catch (err) {
					if (ConnectError.from(err).code === Code.Unauthenticated) lost()
					throw err
				}
			},
		],
	})

	let who: string
	let identity: string
	try {
		const held = await which()
		if (held === null) return askToSignIn()

		who = await whoIs(transport, held.id)
		identity = held.identity
	} catch (err) {
		// A session that ended between the two questions is a sign-in like any
		// other, and so is a server that could not answer: what to do about
		// either is the person's.
		return askToSignIn(String(err))
	}

	const app = await open(transport, identity)

	/**
	 * Letting go of the store: forgetting it first when its person signed out,
	 * and in any case asking again who is signed in. Once, however many calls
	 * come back refused.
	 *
	 * The mirror closes once the forget has reached it, and the page starts
	 * again once it has closed. A close that rejects is a forget that did not
	 * reach the disk: what the person signed out of is still on this browser,
	 * and they are told so rather than left believing it is not.
	 */
	let left = false
	const leave = (forget: boolean): void => {
		if (left) return
		left = true

		void (async () => {
			if (forget) void app.store.forget()
			try {
				await app.store.close()
			} catch {
				return askToSignIn('signed out, but this browser still holds what you saw: clear its data for this site to remove it')
			}

			await boot()
		})()
	}
	drawn = { identity, leave }

	// Refused for its session: let go if the browser's session is not this
	// store's any more. One that still is was refused for something else, and
	// starting the page again would only lose what is on it.
	lost = () => {
		void which().then(
			(held) => {
				if (held?.identity !== identity) leave(false)
			},
			() => {},
		)
	}

	/**
	 * Signing out ends the session on the server -- the key is dead in every
	 * browser that had it -- and drops this caller's copy, as `main.tsx` does,
	 * here and in every other tab holding it.
	 */
	const out = (): void => {
		void send(`${ADDR}/session`, { method: 'DELETE' }).finally(() => {
			signedOut.postMessage(identity)
			leave(true)
		})
	}

	root.render(
		<StrictMode>
			<Provider app={app}>
				<Page who={who} onSignOut={out} />
			</Provider>
		</StrictMode>,
	)
}

void boot()
