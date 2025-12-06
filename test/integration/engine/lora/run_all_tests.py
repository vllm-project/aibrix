#!/usr/bin/env python
"""
Comprehensive LoRA Memory Management Test Runner

This script runs all consolidated LoRA tests and generates a comprehensive report.

Test structure:
1. test_lru_swap_pinning.py - LRU eviction, swap latency, and pinning tests
2. test_memory.py - Memory isolation and budget tests
3. test_batching_concurrency.py - Batching and concurrency tests
4. test_api.py - API idempotency and dynamic loading tests
"""

import subprocess
import sys
import time
import json
import argparse
from datetime import datetime
from pathlib import Path


# Test files
TEST_FILES = [
    "test_lru_swap_pinning.py",
    "test_memory.py",
    "test_batching_concurrency.py",
    "test_api.py",
]


def run_test_file(test_file: str, timeout: int = 600, verbose: bool = True) -> dict:
    """Run a single test file and capture results."""
    print(f"\n{'='*70}")
    print(f"Running: {test_file}")
    print(f"{'='*70}")

    start = time.perf_counter()
    try:
        cmd = [sys.executable, "-m", "pytest", test_file, "-v", "--tb=short"]
        if verbose:
            cmd.append("-s")

        result = subprocess.run(
            cmd,
            capture_output=True,
            text=True,
            timeout=timeout,
            cwd=str(Path(__file__).parent)
        )
        elapsed = time.perf_counter() - start

        # Parse results
        passed = result.stdout.count(" PASSED")
        failed = result.stdout.count(" FAILED")
        errors = result.stdout.count(" ERROR")
        skipped = result.stdout.count(" SKIPPED")

        status = "PASS" if result.returncode == 0 else "FAIL"

        if verbose:
            print(result.stdout)
            if result.stderr:
                print("STDERR:", result.stderr[-2000:])

        return {
            "file": test_file,
            "status": status,
            "passed": passed,
            "failed": failed,
            "errors": errors,
            "skipped": skipped,
            "duration": elapsed,
            "output": result.stdout[-5000:],
            "returncode": result.returncode
        }
    except subprocess.TimeoutExpired:
        return {
            "file": test_file,
            "status": "TIMEOUT",
            "passed": 0,
            "failed": 0,
            "errors": 1,
            "skipped": 0,
            "duration": timeout,
            "output": "Test timed out",
            "returncode": -1
        }
    except Exception as e:
        return {
            "file": test_file,
            "status": "ERROR",
            "passed": 0,
            "failed": 0,
            "errors": 1,
            "skipped": 0,
            "duration": 0,
            "output": str(e),
            "returncode": -1
        }


def generate_report(results: list[dict]) -> str:
    """Generate a markdown test report."""
    report = []
    report.append("# LoRA Memory Management Test Report")
    report.append("")
    report.append(f"**Date:** {datetime.now().strftime('%Y-%m-%d %H:%M:%S')}")
    report.append("")

    # Summary table
    report.append("## Summary")
    report.append("")
    report.append("| Test Suite | Status | Passed | Failed | Skipped | Duration |")
    report.append("|------------|--------|--------|--------|---------|----------|")

    total_passed = 0
    total_failed = 0
    total_skipped = 0
    total_duration = 0

    for r in results:
        status_emoji = "PASS" if r["status"] == "PASS" else "FAIL" if r["status"] == "FAIL" else "TIMEOUT"
        report.append(
            f"| {r['file']} | {status_emoji} | {r['passed']} | {r['failed']} | "
            f"{r.get('skipped', 0)} | {r['duration']:.1f}s |"
        )
        total_passed += r["passed"]
        total_failed += r["failed"]
        total_skipped += r.get("skipped", 0)
        total_duration += r["duration"]

    report.append("")
    report.append(f"**Total:** {total_passed} passed, {total_failed} failed, "
                  f"{total_skipped} skipped in {total_duration:.1f}s")
    report.append("")

    # Test suite descriptions
    report.append("## Test Suites")
    report.append("")
    report.append("### test_lru_swap_pinning.py")
    report.append("- LRU eviction behavior (CPU and GPU tiers)")
    report.append("- CPU-to-GPU swap latency measurements")
    report.append("- LoRA pinning functionality")
    report.append("")
    report.append("### test_memory.py")
    report.append("- Memory isolation between LoRA and KV cache")
    report.append("- Memory budget verification and pre-allocation")
    report.append("- Memory scaling with max_loras and max_lora_rank")
    report.append("")
    report.append("### test_batching_concurrency.py")
    report.append("- Scheduler batching with max_loras limit")
    report.append("- Concurrent request handling")
    report.append("- Burst traffic patterns")
    report.append("")
    report.append("### test_api.py")
    report.append("- add_lora, remove_lora, pin_lora API semantics")
    report.append("- API idempotency behaviors")
    report.append("- Dynamic loading/unloading")
    report.append("")

    # Detailed results
    report.append("## Detailed Results")
    report.append("")

    for r in results:
        report.append(f"### {r['file']}")
        report.append("")
        report.append(f"**Status:** {r['status']}")
        report.append(f"**Duration:** {r['duration']:.2f}s")
        report.append(f"**Passed:** {r['passed']}, **Failed:** {r['failed']}")
        report.append("")

        output = r.get("output", "")

        # Extract key metrics
        metrics_sections = [
            "SWAP LATENCY RESULTS",
            "MEMORY BUDGET ANALYSIS",
            "HIGH CONCURRENCY RESULTS",
            "BATCH SCHEDULING ANALYSIS",
            "THROUGHPUT COMPARISON",
        ]

        for section in metrics_sections:
            if section in output:
                report.append(f"#### {section}")
                report.append("```")
                # Extract section content
                start_idx = output.find(section)
                end_idx = output.find("=" * 60, start_idx + len(section))
                if end_idx == -1:
                    end_idx = start_idx + 500
                section_content = output[start_idx:end_idx].strip()
                report.append(section_content)
                report.append("```")
                report.append("")

        report.append("")

    # Key Findings
    report.append("## Key Findings")
    report.append("")

    all_passed = all(r["status"] == "PASS" for r in results)

    if all_passed:
        report.append("All tests passed! The LoRA memory management system works correctly:")
        report.append("")
        report.append("1. **LRU Eviction**: LoRAs are evicted in LRU order when capacity exceeded")
        report.append("2. **Memory Isolation**: KV cache pressure does NOT evict LoRAs")
        report.append("3. **Pinning**: Pinned LoRAs are protected from eviction")
        report.append("4. **CPU-GPU Swapping**: Automatic swapping works correctly")
        report.append("5. **Batching**: Scheduler correctly limits unique LoRAs per batch")
        report.append("6. **API Idempotency**: APIs behave correctly on repeated calls")
    else:
        report.append("Some tests failed. Review the detailed results above.")
        report.append("")
        failed_tests = [r["file"] for r in results if r["status"] != "PASS"]
        report.append("Failed tests:")
        for t in failed_tests:
            report.append(f"- {t}")

    report.append("")
    report.append("---")
    report.append("*Generated by LoRA Memory Management Test Suite*")

    return "\n".join(report)


def main():
    """Main test runner."""
    parser = argparse.ArgumentParser(description="LoRA Memory Management Test Runner")
    parser.add_argument("--timeout", type=int, default=600,
                        help="Timeout per test file in seconds (default: 600)")
    parser.add_argument("--quiet", action="store_true",
                        help="Reduce output verbosity")

    args = parser.parse_args()

    print("=" * 70)
    print("LoRA Memory Management Test Suite")
    print("=" * 70)
    print(f"Start time: {datetime.now()}")
    print()

    print("Running test suite:")
    for f in TEST_FILES:
        print(f"  - {f}")
    print()

    results = []

    for test_file in TEST_FILES:
        # Check if file exists
        test_path = Path(__file__).parent / test_file
        if not test_path.exists():
            print(f"SKIP: {test_file} not found")
            continue

        result = run_test_file(test_file, timeout=args.timeout, verbose=not args.quiet)
        results.append(result)

        # Save intermediate results
        with open("test_results.json", "w") as f:
            json.dump(results, f, indent=2)

    if not results:
        print("No tests were run!")
        return 1

    # Generate report
    report = generate_report(results)

    # Save report
    report_path = Path(__file__).parent / "TEST_REPORT.md"
    with open(report_path, "w") as f:
        f.write(report)

    print("\n" + "=" * 70)
    print("TEST SUITE COMPLETE")
    print("=" * 70)
    print(f"Report saved to: {report_path}")
    print()

    # Print summary
    total_passed = sum(r["passed"] for r in results)
    total_failed = sum(r["failed"] for r in results)
    print(f"Total: {total_passed} passed, {total_failed} failed")

    # Return exit code
    return 0 if all(r["status"] == "PASS" for r in results) else 1


if __name__ == "__main__":
    sys.exit(main())
