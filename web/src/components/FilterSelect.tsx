import { NativeSelect } from "@/components/ui/input";

// FilterSelect is a list filter whose empty option means no filter; useFilterParams drops the key for it.
export function FilterSelect({
  label,
  all,
  value,
  values,
  onChange,
}: {
  label: string;
  all: string;
  value: string | undefined;
  values: readonly string[];
  onChange: (value: string) => void;
}) {
  return (
    <NativeSelect aria-label={label} value={value ?? ""} onChange={(e) => onChange(e.target.value)}>
      <option value="">{all}</option>
      {values.map((v) => (
        <option key={v} value={v}>
          {v}
        </option>
      ))}
    </NativeSelect>
  );
}
