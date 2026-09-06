import { useState, useEffect, useRef } from "react";
import { useAuth } from "./auth.jsx";
import { Button, Field, Banner } from "./ui/index.js";

export default function Login() {
	const { login, booting, error } = useAuth();
	const [user, setUser] = useState("admin");
	const [pass, setPass] = useState("");
	const [touched, setTouched] = useState(false);
	const passRef = useRef(null);

	useEffect(() => {
		// Move focus to the password field on mount (username defaults to
		// "admin" so the operator only has to type the password).
		passRef.current?.focus();
	}, []);

	async function submit(e) {
		e?.preventDefault();
		setTouched(true);
		await login(user, pass);
	}

	return (
		<div className="login-wrap">
			<form className="login card" onSubmit={submit}>
				<header className="login-head">
					<div className="brand">RMMWay</div>
					<p className="muted">Sign in to continue</p>
				</header>
				<Field
					label="Username"
					value={user}
					onChange={(e) => setUser(e.target.value)}
					autoComplete="username"
					spellCheck={false}
				/>
				<Field
					label="Password"
					ref={passRef}
					type="password"
					value={pass}
					onChange={(e) => setPass(e.target.value)}
					autoComplete="current-password"
				/>
				{touched && error ? <Banner tone="err">{error}</Banner> : null}
				<Button type="submit" variant="primary" busy={booting}>
					{booting ? "Signing in…" : "Sign in"}
				</Button>
			</form>
		</div>
	);
}
