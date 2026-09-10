// Kit loading skeleton: shimmer placeholder for content that hasn't loaded yet.
// Use instead of text "Loading..." states for better perceived performance.
// Supports lines, cards, and avatars.

export default function Skeleton({
  type = "line",
  height = "1rem",
  width = "100%",
  radius = "4px",
  className = "",
}) {
  const base = "skeleton";
  let variant = "";
  switch (type) {
    case "card":
      variant = "skeleton-card";
      break;
    case "avatar":
      variant = "skeleton-avatar";
      break;
    default:
      variant = "skeleton-line";
  }

  const style = {};
  if (type === "line") {
    style.height = height;
    style.width = width;
  }

  return (
    <div
      className={`${base} ${variant} ${className}`}
      style={style}
      role="status"
      aria-label="Loading"
    >
      <span className="skeleton-shimmer" />
    </div>
  );
}

// Table row skeleton for list loading states
export function TableRowSkeleton({ columns = 3 }) {
  return (
    <div className="skeleton-row">
      {Array.from({ length: columns }).map((_, i) => (
        <Skeleton key={i} type="line" height="0.75rem" />
      ))}
    </div>
  );
}

// Card list skeleton for dashboard tiles
export function CardListSkeleton({ count = 3 }) {
  return (
    <div className="skeleton-grid">
      {Array.from({ length: count }).map((_, i) => (
        <Skeleton key={i} type="card" />
      ))}
    </div>
  );
}
