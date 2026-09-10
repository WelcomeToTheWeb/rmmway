// Kit breadcrumb: hierarchical navigation path. Renders as a list of
// links separated by a chevron. The last item is the current page (not a link).
// Usage: <Breadcrumb items={[{label:"Devices", href:"#\/devices"}, {label:"PiCodingWay"}]} />
export default function Breadcrumb({ items }) {
  if (!items || items.length === 0) return null;
  return (
    <nav className="breadcrumb" aria-label="Breadcrumb">
      <ol>
        {items.map((item, i) => {
          const last = i === items.length - 1;
          if (last || !item.href) {
            return (
              <li key={i} className="breadcrumb-item">
                <span className="breadcrumb-current">{item.label}</span>
              </li>
            );
          }
          return (
            <li key={i} className="breadcrumb-item">
              <a className="breadcrumb-link" href={item.href}>
                {item.label}
              </a>
            </li>
          );
        })}
      </ol>
    </nav>
  );
}
