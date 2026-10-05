import unittest

from generator.dataset_generator.synthetic_prompt import (
    _truncate_to_target_length,
    adjust_prompt_length,
    generate_synthetic_prompt,
)


class _ExpandingTokenizer:
    """
    Fake tokenizer where token id 5 decodes to two words ("w5 bonus")
    instead of one. Re-encoding that decoded text therefore yields more
    tokens than were originally truncated to, mirroring how a real BPE/
    sentencepiece tokenizer can split a multi-byte character or merge at a
    truncation boundary and overshoot the requested length on re-encode.
    """

    def encode(self, text, add_special_tokens=True):
        ids = []
        for word in text.split():
            if word == "bonus":
                ids.append(6)
            else:
                ids.append(int(word[1:]))
        return ids

    def decode(self, ids, skip_special_tokens=True):
        words = []
        for token_id in ids:
            if token_id == 5:
                words.append("w5 bonus")
            else:
                words.append(f"w{token_id}")
        return " ".join(words)


class _WordTokenizer:
    """Simple whitespace tokenizer used to check returned token counts."""

    def encode(self, text, add_special_tokens=True):
        return text.split()

    def decode(self, ids, skip_special_tokens=True):
        return " ".join(ids)


class TruncateToTargetLengthTest(unittest.TestCase):
    def test_shrinks_further_when_decode_reencode_overshoots(self):
        tokenizer = _ExpandingTokenizer()
        # ids[:5] decodes to "w1 w2 w3 w4 w5 bonus", which re-encodes to 6
        # tokens -- one more than requested.
        token_ids = [1, 2, 3, 4, 5, 6, 7]
        text, token_count = _truncate_to_target_length(tokenizer, token_ids, 5)
        self.assertLessEqual(token_count, 5)
        self.assertLessEqual(len(tokenizer.encode(text)), 5)

    def test_no_op_when_round_trip_already_fits(self):
        tokenizer = _WordTokenizer()
        token_ids = ["w1", "w2", "w3", "w4", "w5", "w6"]
        text, token_count = _truncate_to_target_length(tokenizer, token_ids, 4)
        self.assertEqual(text, "w1 w2 w3 w4")
        self.assertEqual(token_count, 4)


class AdjustPromptLengthTest(unittest.TestCase):
    def test_truncated_prompt_never_exceeds_target_on_reencode(self):
        tokenizer = _ExpandingTokenizer()
        prompt = "w1 w2 w3 w4 w5 w6 w7"
        adjusted = adjust_prompt_length(tokenizer, prompt, target_token_length=5)
        self.assertLessEqual(len(tokenizer.encode(adjusted)), 5)

    def test_truncates_after_padding_overshoots_target(self):
        # A short prompt is padded in whole-sentence chunks, so it can jump
        # past target_token_length in a single append. Truncation must still
        # run afterwards to bring it back down to the target.
        tokenizer = _WordTokenizer()
        prompt = "w1 w2"
        adjusted = adjust_prompt_length(tokenizer, prompt, target_token_length=5)
        self.assertLessEqual(len(tokenizer.encode(adjusted)), 5)


class GenerateSyntheticPromptTest(unittest.TestCase):
    def test_returned_token_count_matches_returned_prompt(self):
        tokenizer = _WordTokenizer()
        prompt, token_count = generate_synthetic_prompt(
            tokenizer, target_token_length=5
        )
        self.assertEqual(token_count, len(tokenizer.encode(prompt)))
        self.assertLessEqual(token_count, 5)


if __name__ == "__main__":
    unittest.main()
