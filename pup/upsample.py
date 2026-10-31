"""Expand a prompt into the caption FLUX.3 Image Klein is trained on, with its prompt upsampler.

iris studio runs this as a subprocess, once per caption, and exiting is what frees the
memory. JSON on stdin: model, text_encoder, prompt, references, boxes, width, height.
JSON on stdout: caption, tokens. What goes wrong ends in an "Error: " line on stderr,
in iris's own words for it, because the studio reads this the way it reads iris.
"""

from __future__ import annotations

import contextlib
import json
import math
import os
import sys
import traceback

os.environ["HF_HUB_OFFLINE"] = "1"  # both models are paths on this machine
os.environ["TQDM_DISABLE"] = "1"  # hundreds of lines of it would land in the studio's terminal


def request_with_canvas(prompt: str, width: int, height: int, references: list[str]) -> str:
    """The prompt, led by the canvas it is for. The t2i and t2i_bbox system prompts take this line, the two i2i ones do not."""
    if references or not (width and height):
        return prompt
    gcd = math.gcd(width, height)
    return f"TARGET CANVAS: {width // gcd}:{height // gcd}\n{prompt}"


def upsample(prompt: str, references: list[str], boxes: list[dict] | None, model_path: str, device: str = "mps") -> str:
    """Return the caption. Loads the upsampler on `device`."""
    import torch
    from diffusers.modular_pipelines import Flux3DistilledAutoBlocks  # type: ignore[import-not-found]
    from PIL import Image
    from transformers import AutoProcessor, Qwen3_5ForConditionalGeneration

    # the same block for the distilled model and Base
    up = Flux3DistilledAutoBlocks().sub_blocks.pop("prompt_upsampler").init_pipeline()
    model = Qwen3_5ForConditionalGeneration.from_pretrained(model_path, dtype=torch.bfloat16, device_map=device).eval()
    up.update_components(
        prompt_upsampler=model,
        prompt_upsampler_processor=AutoProcessor.from_pretrained(model_path),
    )

    inputs = {}
    if references:
        inputs["image"] = [Image.open(path) for path in references]
    if boxes:
        inputs["boxes"] = boxes
    return up(prompt=prompt, **inputs, output="prompt")


def count_tokens(caption: str, text_encoder: str) -> int:
    """The tokens iris gives the caption: qwen3_tokenize_chat's template for FLUX.3, which has no think tags, counts."""
    from transformers import AutoTokenizer

    tokenizer = AutoTokenizer.from_pretrained(text_encoder)
    text = f"<|im_start|>user\n{caption}<|im_end|>\n<|im_start|>assistant\n"
    return len(tokenizer(text).input_ids)


def main() -> int:
    try:
        job = json.load(sys.stdin)
        references = job.get("references") or []
        prompt = request_with_canvas(job["prompt"], job.get("width", 0), job.get("height", 0), references)
        # stdout is the answer, and a library that prints would corrupt it
        with contextlib.redirect_stdout(sys.stderr):
            caption = upsample(prompt, references, job.get("boxes"), job["model"])
            tokens = count_tokens(caption, job["text_encoder"])
    except Exception as err:
        traceback.print_exc()
        print("Error:", " ".join(str(err).split()), file=sys.stderr)
        return 1
    json.dump({"caption": caption, "tokens": tokens}, sys.stdout)
    return 0


if __name__ == "__main__":
    sys.exit(main())
