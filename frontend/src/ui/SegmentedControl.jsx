// Kit segmented control: a compact radio-style group (`.seg`/`.seg-item`).
export default function SegmentedControl({
	options,
	value,
	onChange,
	disabled,
	className = "",
}) {
	// options: [{ value, label }]
	return (
		<span
			className={"seg" + (className ? " " + className : "")}
			role="radiogroup"
		>
			{options.map((o) => (
				<button
					key={String(o.value)}
					type="button"
					role="radio"
					aria-checked={o.value === value}
					className={"seg-item" + (o.value === value ? " active" : "")}
					disabled={disabled}
					onClick={() => onChange(o.value)}
				>
					{o.label}
				</button>
			))}
		</span>
	);
}
