// Kit form field: the existing `.field` markup
// (<label class="field"><span>label</span><input …/></label>) with optional
// hint/error lines. `as` switches the control (input/select/textarea);
// forwards ref so callers can focus the control.
import { forwardRef } from "react";

const Field = forwardRef(function Field(
	{ label, hint, error, as: Comp = "input", className = "", ...rest },
	ref,
) {
	return (
		<label className={"field" + (className ? " " + className : "")}>
			<span>{label}</span>
			<Comp ref={ref} {...rest} />
			{hint && !error && <span className="field-hint">{hint}</span>}
			{error && <span className="row-err">{error}</span>}
		</label>
	);
});

export default Field;
