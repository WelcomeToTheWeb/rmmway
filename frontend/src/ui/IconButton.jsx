// Kit icon button: square transparent button (`.icon-btn`) with an SVG icon
// and an accessible label.
import { Icon } from "./Icon.jsx";

export default function IconButton({
	name,
	label,
	size,
	className = "",
	children,
	...rest
}) {
	return (
		<button
			type="button"
			className={"icon-btn" + (className ? " " + className : "")}
			aria-label={label}
			title={label}
			{...rest}
		>
			{children || <Icon name={name} size={size} />}
		</button>
	);
}
