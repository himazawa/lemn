import type { ExtensionAPI } from "@earendil-works/pi-coding-agent";
import lemnExtension from "./index";

export default function lemnReadOnly(pi: ExtensionAPI) {
  lemnExtension(pi, { readOnly: true });
}